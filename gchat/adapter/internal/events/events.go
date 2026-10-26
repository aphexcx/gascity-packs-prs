// Package events reconciles the identity's Workspace Events subscription.
package events

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gchat-adapter/internal/ledger"
	"google.golang.org/api/googleapi"
	api "google.golang.org/api/workspaceevents/v1"
)

const Target = "//chat.googleapis.com/spaces/-"

// EventTypes excludes batch and subscription lifecycle types: Google includes
// those automatically. New copies this list, so callers must not mutate it.
var EventTypes = []string{
	"google.workspace.chat.message.v1.created",
	"google.workspace.chat.message.v1.updated",
	"google.workspace.chat.message.v1.deleted",
	"google.workspace.chat.reaction.v1.created",
	"google.workspace.chat.reaction.v1.deleted",
	"google.workspace.chat.membership.v1.created",
	"google.workspace.chat.membership.v1.updated",
	"google.workspace.chat.membership.v1.deleted",
}

const operationTimeout = 2 * time.Minute

// Clock allows deterministic operation polling and renewal scheduling. Sleep
// must return promptly on cancellation. Now must report wall time, including
// clock corrections, rather than time elapsed since process startup.
type Clock interface {
	Now() time.Time
	Sleep(context.Context, time.Duration) error
}

type wallClock struct{}

func (wallClock) Now() time.Time { return time.Now().UTC() }
func (wallClock) Sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

type Options struct {
	Service    *api.Service
	Topic      string
	Ledger     *ledger.Ledger
	LedgerPath string
	// LedgerMu must also guard every other component's use of this ledger.
	// The manager never holds it during network calls or sleeps.
	LedgerMu *sync.Mutex
	Clock    Clock        // nil uses the system clock
	Log      *slog.Logger // nil uses slog.Default
}

type Manager struct {
	service  *api.Service
	topic    string
	types    []string
	ledger   *ledger.Ledger
	path     string
	ledgerMu *sync.Mutex
	clock    Clock
	log      *slog.Logger
	// gate serializes renewals and lifecycle deliveries, including their reads.
	gate            chan struct{}
	recoveryNeeded  chan struct{}
	failureRecovery chan struct{}
	pending         *api.Operation
	pendingKind     string
	scopeReloaded   bool
	reloadPending   atomic.Bool
	mutationStarted time.Time
	ambiguousUntil  time.Time
	ambiguousState  ledger.SubscriptionState
	ambiguousCause  error
	lastError       error
	failures        int
	retryDelay      time.Duration
	nextAttempt     time.Time
	scheduledName   string
	scheduledExpiry time.Time
	renewAt         time.Time
	statusMu        sync.Mutex
	status          statusSnapshot
}

type statusSnapshot struct {
	err      error
	failures int
	next     time.Time
	state    ledger.SubscriptionState
}

func New(opts Options) (*Manager, error) {
	if opts.Service == nil || opts.Topic == "" || opts.Ledger == nil || opts.LedgerPath == "" || opts.LedgerMu == nil {
		return nil, errors.New("events: service, topic, ledger, ledger path and shared ledger mutex are required")
	}
	if opts.Clock == nil {
		opts.Clock = wallClock{}
	}
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	opts.LedgerMu.Lock()
	state := opts.Ledger.Subscription
	opts.LedgerMu.Unlock()
	return &Manager{
		service: opts.Service, topic: opts.Topic, types: slices.Clone(EventTypes),
		ledger: opts.Ledger, path: opts.LedgerPath, ledgerMu: opts.LedgerMu,
		clock: opts.Clock, log: opts.Log, gate: make(chan struct{}, 1),
		recoveryNeeded:  make(chan struct{}, 1),
		failureRecovery: make(chan struct{}, 1),
		status:          statusSnapshot{state: state},
	}, nil
}

// RecoveryNeeded signals that a creation or reactivation was durably observed.
// The app must consume this channel to schedule catch-up, including recoveries
// performed by RenewLoop. Signals coalesce while catch-up is pending; the app
// must also perform startup catch-up because notifications are not durable.
func (m *Manager) RecoveryNeeded() <-chan struct{} { return m.recoveryNeeded }

// FailureRecovered signals a successful reconciliation after failures. Signals
// coalesce until consumed and survive subsequent failures, so status polling
// cannot miss an incident boundary. This signal is independent of catch-up.
func (m *Manager) FailureRecovered() <-chan struct{} { return m.failureRecovery }

// Reconcile mutates only after an authoritative get or discovery.
// Errors (including lost responses) leave the caller responsible for retrying
// with backoff; RenewLoop does this automatically. No ambiguous read is absence.
func (m *Manager) Reconcile(ctx context.Context) (ledger.SubscriptionState, error) {
	return m.reconcile(ctx)
}

// TokenReloaded is called by the app after validating and installing a new
// credential. It permits scope-revoked reactivation (including transient retries) under
// the same gate as lifecycle events and RenewLoop. The reload remains latched
// if this caller is cancelled while waiting for another reconciliation.
func (m *Manager) TokenReloaded(ctx context.Context) (ledger.SubscriptionState, error) {
	m.reloadPending.Store(true)
	return m.reconcile(ctx)
}

func (m *Manager) reconcile(ctx context.Context) (_ ledger.SubscriptionState, resultErr error) {
	select {
	case m.gate <- struct{}{}:
		defer func() { <-m.gate }()
	case <-ctx.Done():
		return ledger.SubscriptionState{}, ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return ledger.SubscriptionState{}, err
	}
	defer func() {
		if ctx.Err() == nil {
			m.recordResult(resultErr)
		}
	}()
	if m.reloadPending.Swap(false) {
		m.scopeReloaded = true
	}
	m.ledgerMu.Lock()
	saved := m.ledger.Subscription
	m.ledgerMu.Unlock()
	var sub *api.Subscription
	var err error
	// Resume before looking up the resource: a resource snapshot cannot finish
	// an Operation, and validation of that snapshot must not block its poll.
	if m.pending != nil {
		kind := m.pendingKind
		state, err := m.resume(ctx)
		if err == nil {
			// An operation result can predate a later suspension or deletion.
			var lookupErr error
			sub, lookupErr = m.lookup(ctx, state.Name)
			if lookupErr != nil {
				return state, lookupErr
			}
			if sub != nil {
				return m.reconcileExisting(ctx, sub)
			}
		} else {
			if kind == "create" || ctx.Err() != nil {
				return saved, err
			}
			// A renewal/reactivation of a vanished resource cannot repair the
			// lapse. Only confirmed absence permits abandoning that operation.
			var lookupErr error
			sub, lookupErr = m.lookup(ctx, saved.Name)
			if lookupErr != nil {
				return saved, lookupErr
			}
			if sub != nil {
				state, persistErr := m.persist(sub, time.Time{})
				return state, m.reportSuspension(ctx, sub, errors.Join(err, persistErr))
			}
		}
		m.pending = nil
	} else {
		sub, err = m.lookup(ctx, saved.Name)
		if err != nil {
			return saved, err
		}
	}
	if sub == nil {
		if m.clock.Now().Before(m.ambiguousUntil) {
			return saved, m.holdError("events: awaiting discovery after a failed mutation")
		}
		m.mutationStarted = m.clock.Now()
		m.pendingKind = "create"
		op, err := m.service.Subscriptions.Create(&api.Subscription{TargetResource: Target, EventTypes: slices.Clone(m.types), PayloadOptions: &api.PayloadOptions{IncludeResource: true}, NotificationEndpoint: &api.NotificationEndpoint{PubsubTopic: m.topic}}).Context(ctx).Do()
		if isHTTP(err, http.StatusConflict) {
			return m.adopt(ctx)
		}
		if err != nil {
			m.ambiguousUntil = m.clock.Now().Add(operationTimeout)
			m.ambiguousState = ledger.SubscriptionState{}
			m.ambiguousCause = fmt.Errorf("events: create: %w", err)
			return saved, m.ambiguousCause
		}
		return m.finish(ctx, op)
	}
	return m.reconcileExisting(ctx, sub)
}

func (m *Manager) lookup(ctx context.Context, name string) (*api.Subscription, error) {
	if name != "" {
		sub, err := m.service.Subscriptions.Get(name).Context(ctx).Do()
		if err == nil {
			if !m.expiredOrDeleted(sub) {
				if _, err := m.validate(sub); err != nil {
					return nil, err
				}
				return sub, nil
			}
		} else if !isHTTP(err, http.StatusNotFound) {
			return nil, fmt.Errorf("events: get: %w", err)
		}
	}
	return m.discover(ctx)
}

func (m *Manager) discover(ctx context.Context) (*api.Subscription, error) {
	terms := make([]string, len(m.types))
	for i, typ := range m.types {
		terms[i] = fmt.Sprintf("event_types:%q", typ)
	}
	filter := fmt.Sprintf("(%s) AND target_resource=%q", strings.Join(terms, " OR "), Target)
	var found *api.Subscription
	err := m.service.Subscriptions.List().Filter(filter).Pages(ctx, func(page *api.ListSubscriptionsResponse) error {
		for _, sub := range page.Subscriptions {
			if m.expiredOrDeleted(sub) {
				continue
			}
			// Check all pages and the complete configuration. Silently adopting
			// an incompatible topic or partial event set would lose deliveries.
			if _, err := m.validate(sub); err != nil {
				return err
			}
			if found != nil && found.Name != sub.Name {
				return errors.New("events: multiple matching subscriptions; refusing to choose")
			}
			found = sub
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("events: discover: %w", err)
	}
	return found, nil
}

func (m *Manager) expiredOrDeleted(sub *api.Subscription) bool {
	if sub == nil {
		return false // validate must reject a malformed live record.
	}
	if sub.State == "DELETED" {
		return true
	}
	expiry, err := time.Parse(time.RFC3339Nano, sub.ExpireTime)
	return err == nil && !m.clock.Now().Before(expiry)
}

func (m *Manager) adopt(ctx context.Context) (ledger.SubscriptionState, error) {
	sub, err := m.discover(ctx)
	if err != nil {
		return ledger.SubscriptionState{}, err
	}
	if sub == nil {
		m.ambiguousUntil = m.clock.Now().Add(operationTimeout)
		m.ambiguousState = ledger.SubscriptionState{}
		m.ambiguousCause = errors.New("events: ALREADY_EXISTS but subscription is not yet discoverable")
		return ledger.SubscriptionState{}, m.ambiguousCause
	}
	return m.reconcileExisting(ctx, sub)
}

func (m *Manager) reconcileExisting(ctx context.Context, sub *api.Subscription) (state ledger.SubscriptionState, err error) {
	m.reportSuspension(ctx, sub, nil)
	defer func() { err = suspensionError(sub, err) }()
	if sub.State == "ACTIVE" {
		m.scopeReloaded = false
	}
	m.ledgerMu.Lock()
	adopting := m.ledger.Subscription.Name != sub.Name
	m.ledgerMu.Unlock()
	state, err = m.persist(sub, time.Time{})
	if err != nil {
		return state, err
	}
	if adopting {
		m.logMutation(ctx, "adopt", state)
	}
	if state != m.ambiguousState {
		m.ambiguousUntil = time.Time{}
		m.ambiguousCause = nil
	}
	if sub.Reconciling {
		return state, errors.New("events: subscription is still reconciling")
	}
	if sub.State == "ACTIVE" && m.clock.Now().Before(m.renewAt) {
		return state, nil
	}
	if m.clock.Now().Before(m.ambiguousUntil) {
		return state, m.holdError("events: awaiting authoritative change after a failed mutation")
	}
	m.mutationStarted = m.clock.Now()
	var op *api.Operation
	switch sub.State {
	case "ACTIVE":
		m.pendingKind = "renew"
		op, err = m.service.Subscriptions.Patch(sub.Name, &api.Subscription{Ttl: "0s"}).UpdateMask("ttl").Context(ctx).Do()
	case "SUSPENDED":
		if sub.SuspensionReason == "USER_SCOPE_REVOKED" || sub.SuspensionReason == "APP_SCOPE_REVOKED" {
			if !m.scopeReloaded {
				return state, errors.New("events: awaiting validated token reload before reactivation")
			}
		}
		m.pendingKind = "reactivate"
		op, err = m.service.Subscriptions.Reactivate(sub.Name, &api.ReactivateSubscriptionRequest{}).Context(ctx).Do()
	default:
		return state, fmt.Errorf("events: unsupported subscription state %q", sub.State)
	}
	if err != nil {
		if m.pendingKind == "reactivate" && (isHTTP(err, http.StatusForbidden) || isHTTP(err, http.StatusUnauthorized)) {
			m.scopeReloaded = false
		}
		m.ambiguousUntil = m.clock.Now().Add(operationTimeout)
		m.ambiguousState = state
		m.ambiguousCause = fmt.Errorf("events: reconcile %s: %w", sub.Name, err)
		return state, m.ambiguousCause
	}
	return m.finish(ctx, op)
}

// resume makes at most one bounded HTTP poll per Reconcile. Retained operations
// never consume another two-minute polling loop; callers provide retry backoff.
func (m *Manager) resume(ctx context.Context) (ledger.SubscriptionState, error) {
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	op, err := m.service.Operations.Get(m.pending.Name).Context(ctx).Do()
	if err != nil {
		if isHTTP(err, http.StatusNotFound) {
			m.pending = nil
		}
		return ledger.SubscriptionState{}, fmt.Errorf("events: resume operation: %w", err)
	}
	m.pending = op
	if !op.Done {
		return ledger.SubscriptionState{}, fmt.Errorf("events: operation %s is still pending", op.Name)
	}
	return m.finish(ctx, op)
}

func (m *Manager) finish(ctx context.Context, op *api.Operation) (ledger.SubscriptionState, error) {
	// The context bounds a stuck HTTP request; the injected clock bounds polling
	// without requiring tests to wait two real minutes.
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	deadline := m.clock.Now().Add(operationTimeout)
	backoff := time.Second
	for !op.Done {
		if op.Name == "" {
			return ledger.SubscriptionState{}, errors.New("events: pending operation has no name")
		}
		m.pending = op
		remaining := deadline.Sub(m.clock.Now())
		if remaining <= 0 {
			return ledger.SubscriptionState{}, fmt.Errorf("events: operation %s: %w", op.Name, context.DeadlineExceeded)
		}
		if err := m.clock.Sleep(ctx, min(backoff, remaining)); err != nil {
			return ledger.SubscriptionState{}, err
		}
		next, err := m.service.Operations.Get(op.Name).Context(ctx).Do()
		if err != nil {
			if isHTTP(err, http.StatusNotFound) {
				// Operation retention is not subscription retention. The next
				// attempt must reconcile the resource instead of polling forever.
				m.pending = nil
			}
			return ledger.SubscriptionState{}, fmt.Errorf("events: poll %s: %w", op.Name, err)
		}
		op = next
		backoff = min(backoff*2, 10*time.Second)
	}
	m.pending = nil
	if op.Error != nil {
		if m.pendingKind == "reactivate" && (op.Error.Code == 7 || op.Error.Code == 16) {
			m.scopeReloaded = false
		}
		if op.Error.Code == 6 {
			return m.adopt(ctx)
		} // google.rpc.Code.ALREADY_EXISTS
		return ledger.SubscriptionState{}, fmt.Errorf("events: operation failed (%d): %s", op.Error.Code, op.Error.Message)
	}
	var sub api.Subscription
	if err := json.Unmarshal(op.Response, &sub); err != nil {
		return ledger.SubscriptionState{}, fmt.Errorf("events: decode operation: %w", err)
	}
	state, err := m.persist(&sub, m.mutationStarted)
	if err != nil {
		return state, err
	}
	m.ambiguousUntil = time.Time{}
	m.ambiguousCause = nil
	m.logMutation(ctx, m.pendingKind, state)
	if state.State != "ACTIVE" || sub.Reconciling || !m.clock.Now().Before(state.Expiry) {
		return state, errors.New("events: completed operation did not leave an active, unexpired subscription")
	}
	if m.pendingKind == "renew" && !m.clock.Now().Before(m.renewAt) {
		return state, errors.New("events: renewal did not extend the subscription")
	}
	m.scopeReloaded = false
	return state, nil
}

func (m *Manager) validate(sub *api.Subscription) (time.Time, error) {
	if sub == nil {
		return time.Time{}, errors.New("events: subscription is <nil>")
	}
	incompatible := func(field string, value any) (time.Time, error) {
		return time.Time{}, fmt.Errorf("events: subscription %q has incompatible %s=%v; remove the incompatible subscription manually", sub.Name, field, value)
	}
	switch {
	case sub.Name == "":
		return incompatible("name", `""`)
	case sub.TargetResource != Target:
		return incompatible("targetResource", sub.TargetResource)
	case sub.NotificationEndpoint == nil:
		return incompatible("notificationEndpoint", nil)
	case sub.NotificationEndpoint.PubsubTopic != m.topic:
		return incompatible("notificationEndpoint.pubsubTopic", sub.NotificationEndpoint.PubsubTopic)
	case sub.PayloadOptions == nil:
		return incompatible("payloadOptions", nil)
	case !sub.PayloadOptions.IncludeResource:
		return incompatible("payloadOptions.includeResource", sub.PayloadOptions.IncludeResource)
	}
	// The API's patch updateMask only permits expiry, TTL and event types;
	// payload options cannot be repaired here. Refuse adoption (and duplicate
	// creation) until the operator removes the incompatible subscription.
	if sub.PayloadOptions.FieldMask != "" {
		return incompatible("payloadOptions.fieldMask", sub.PayloadOptions.FieldMask)
	}
	for _, typ := range m.types {
		if !slices.Contains(sub.EventTypes, typ) {
			return incompatible("eventTypes", fmt.Sprintf("%v (missing %s)", sub.EventTypes, typ))
		}
	}
	expiry, err := time.Parse(time.RFC3339Nano, sub.ExpireTime)
	if err != nil {
		return time.Time{}, fmt.Errorf("events: subscription %q has invalid expireTime=%q: %w", sub.Name, sub.ExpireTime, err)
	}
	return expiry, nil
}

func (m *Manager) persist(sub *api.Subscription, started time.Time) (ledger.SubscriptionState, error) {
	expiry, err := m.validate(sub)
	if err != nil {
		return ledger.SubscriptionState{}, err
	}
	state := ledger.SubscriptionState{Name: sub.Name, Target: sub.TargetResource, State: sub.State, Expiry: expiry}
	m.ledgerMu.Lock()
	previous := m.ledger.Subscription
	if previous != state {
		m.ledger.Subscription = state
		err = m.ledger.Save(m.path)
		if err != nil {
			m.ledger.Subscription = previous
		}
	}
	m.ledgerMu.Unlock()
	if err != nil {
		return previous, fmt.Errorf("events: persist subscription: %w", err)
	}
	if state.State == "ACTIVE" && (previous.Name != state.Name || previous.State == "SUSPENDED") {
		select {
		case m.recoveryNeeded <- struct{}{}:
		default:
		}
	}
	if m.scheduledName != sub.Name || !m.scheduledExpiry.Equal(expiry) {
		// A create/renew's start time measures the returned TTL even when its
		// Operation takes a while. Discovery can use Google's update/create
		// timestamps; old ledgers lack these, so use the four-hour maximum.
		if started.IsZero() {
			started, _ = time.Parse(time.RFC3339Nano, sub.UpdateTime)
			if started.IsZero() {
				started, _ = time.Parse(time.RFC3339Nano, sub.CreateTime)
			}
		}
		ttl := 4 * time.Hour
		if !started.IsZero() && expiry.After(started) {
			ttl = min(expiry.Sub(started), ttl)
		}
		m.renewAt = expiry.Add(-ttl / 4)
		m.scheduledName, m.scheduledExpiry = sub.Name, expiry
		// A cancelled caller skips recordResult, but a newly observed earlier
		// expiry must still reach RenewLoop. An unchanged GET must preserve
		// shared retry backoff even if its renewal time is already in the past.
		if m.renewAt.Before(m.nextAttempt) {
			m.nextAttempt = m.renewAt
		}
	}
	m.statusMu.Lock()
	m.status = statusSnapshot{err: m.lastError, failures: m.failures, next: m.nextAttempt, state: state}
	m.statusMu.Unlock()
	return state, nil
}

func (m *Manager) reportSuspension(ctx context.Context, sub *api.Subscription, err error) error {
	if sub.State == "SUSPENDED" {
		m.log.WarnContext(ctx, "subscription suspended", "name", sub.Name, "state", sub.State, "reason", sub.SuspensionReason)
	}
	return suspensionError(sub, err)
}

func suspensionError(sub *api.Subscription, err error) error {
	if sub.State == "SUSPENDED" && err != nil {
		return fmt.Errorf("events: subscription %s suspended (%s): %w", sub.Name, sub.SuspensionReason, err)
	}
	return err
}

func (m *Manager) logMutation(ctx context.Context, action string, state ledger.SubscriptionState) {
	m.log.InfoContext(ctx, "subscription mutation", "action", action, "name", state.Name, "state", state.State, "expiry", state.Expiry)
}

// Status returns the last reconciliation error, consecutive failures, scheduled
// next attempt, and last durably observed subscription. Its snapshot lock is
// independent of reconciliation and persistence, so reads never wait for I/O.
// Before the first attempt the error, count, and next attempt are zero.
// RenewLoop consumes this schedule; direct Reconcile callers remain responsible
// for retrying.
//
//lint:ignore ST1008 The first value is a stored diagnostic, not an accessor failure; preserve the status contract.
func (m *Manager) Status() (error, int, time.Time, ledger.SubscriptionState) {
	m.statusMu.Lock()
	defer m.statusMu.Unlock()
	return m.status.err, m.status.failures, m.status.next, m.status.state
}

// recordResult and holdError are only called under gate.
func (m *Manager) recordResult(err error) {
	recovered := err == nil && m.failures > 0
	defer func() {
		m.statusMu.Lock()
		m.status.err, m.status.failures, m.status.next = m.lastError, m.failures, m.nextAttempt
		if recovered {
			select {
			case m.failureRecovery <- struct{}{}:
			default:
			}
		}
		m.statusMu.Unlock()
	}()
	m.lastError = err
	if err == nil {
		m.failures, m.retryDelay = 0, 0
		m.nextAttempt = m.renewAt
		return
	}
	m.failures++
	if m.retryDelay == 0 {
		m.retryDelay = time.Second
	} else {
		m.retryDelay = min(2*m.retryDelay, operationTimeout)
	}
	m.nextAttempt = m.clock.Now().Add(m.retryDelay)
}

func (m *Manager) holdError(message string) error {
	if m.ambiguousCause != nil {
		return fmt.Errorf("%s: %w", message, m.ambiguousCause)
	}
	return errors.New(message)
}

// RenewLoop retries failures with exponential backoff up to two minutes. Long
// waits recheck wall time and the latest schedule at least once a minute, so
// clock jumps and lifecycle-driven renewal cannot leave a stale relative timer.
func (m *Manager) RenewLoop(ctx context.Context) {
	for ctx.Err() == nil {
		_, err := m.Reconcile(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			m.log.ErrorContext(ctx, "subscription reconciliation failed", "cause", err)
		}
		// Both recovery and retry schedules can change after Reconcile returns.
		// Use the latest result each minute, including a concurrent success
		// that replaces backoff with a renewal hours in the future.
		for ctx.Err() == nil {
			_, _, next, _ := m.Status()
			delay := next.Sub(m.clock.Now())
			if delay <= 0 {
				break
			}
			if m.clock.Sleep(ctx, min(delay, time.Minute)) != nil {
				return
			}
		}
	}
}

// HandleLifecycle ignores unrelated events. It deliberately ignores the event
// payload's state: lifecycle deliveries can be late, duplicated or unordered.
// Both full CloudEvent types and normalized subscription.<kind> are accepted.
// Returning errors lets the dispatcher nack/retry a transient failure.
func (m *Manager) HandleLifecycle(ctx context.Context, eventType string) error {
	switch eventType {
	case "google.workspace.events.subscription.v1.expirationReminder", "google.workspace.events.subscription.v1.expired", "google.workspace.events.subscription.v1.suspended",
		"subscription.expirationReminder", "subscription.expired", "subscription.suspended":
		_, err := m.Reconcile(ctx)
		return err
	default:
		return nil
	}
}

func isHTTP(err error, code int) bool {
	var apiErr *googleapi.Error
	return errors.As(err, &apiErr) && apiErr.Code == code
}

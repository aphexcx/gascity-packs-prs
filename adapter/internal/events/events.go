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
	pending         *api.Operation
	mutationStarted time.Time
	ambiguousUntil  time.Time
	scheduledName   string
	scheduledExpiry time.Time
	renewAt         time.Time
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
	return &Manager{service: opts.Service, topic: opts.Topic, types: slices.Clone(EventTypes), ledger: opts.Ledger, path: opts.LedgerPath, ledgerMu: opts.LedgerMu, clock: opts.Clock, log: opts.Log, gate: make(chan struct{}, 1)}, nil
}

// Reconcile makes at most one mutation, after an authoritative get or discovery.
// Errors (including lost responses) leave the caller responsible for retrying
// with backoff; RenewLoop does this automatically. No ambiguous read is absence.
func (m *Manager) Reconcile(ctx context.Context) (ledger.SubscriptionState, error) {
	select {
	case m.gate <- struct{}{}:
		defer func() { <-m.gate }()
	case <-ctx.Done():
		return ledger.SubscriptionState{}, ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return ledger.SubscriptionState{}, err
	}
	m.ledgerMu.Lock()
	saved := m.ledger.Subscription
	m.ledgerMu.Unlock()
	sub, err := m.lookup(ctx, saved.Name)
	if err != nil {
		return saved, err
	}
	// A known operation survives a polling timeout in memory. First reconcile
	// Google's resource, then resume waiting instead of issuing another mutation.
	if m.pending != nil {
		return m.finish(ctx, m.pending)
	}
	if sub == nil {
		if m.clock.Now().Before(m.ambiguousUntil) {
			return saved, errors.New("events: awaiting discovery after an ambiguous mutation")
		}
		m.mutationStarted = m.clock.Now()
		op, err := m.service.Subscriptions.Create(&api.Subscription{TargetResource: Target, EventTypes: slices.Clone(m.types), PayloadOptions: &api.PayloadOptions{IncludeResource: true}, NotificationEndpoint: &api.NotificationEndpoint{PubsubTopic: m.topic}}).Context(ctx).Do()
		if isHTTP(err, http.StatusConflict) {
			return m.adopt(ctx)
		}
		if err != nil {
			m.ambiguousUntil = m.clock.Now().Add(operationTimeout)
			return saved, fmt.Errorf("events: create: %w", err)
		}
		return m.finish(ctx, op)
	}
	return m.reconcileExisting(ctx, sub)
}

func (m *Manager) lookup(ctx context.Context, name string) (*api.Subscription, error) {
	if name != "" {
		sub, err := m.service.Subscriptions.Get(name).Context(ctx).Do()
		if err == nil {
			expiry, err := m.validate(sub)
			if err != nil {
				return nil, err
			}
			if sub.State != "DELETED" && m.clock.Now().Before(expiry) {
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
			// Check all pages and the complete configuration. Silently adopting
			// an incompatible topic or partial event set would lose deliveries.
			expiry, err := m.validate(sub)
			if err != nil {
				return err
			}
			if sub.State == "DELETED" || !m.clock.Now().Before(expiry) {
				continue
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

func (m *Manager) adopt(ctx context.Context) (ledger.SubscriptionState, error) {
	sub, err := m.discover(ctx)
	if err != nil {
		return ledger.SubscriptionState{}, err
	}
	if sub == nil {
		return ledger.SubscriptionState{}, errors.New("events: ALREADY_EXISTS but subscription is not yet discoverable")
	}
	return m.reconcileExisting(ctx, sub)
}

func (m *Manager) reconcileExisting(ctx context.Context, sub *api.Subscription) (ledger.SubscriptionState, error) {
	state, err := m.persist(sub, time.Time{})
	if err != nil {
		return state, err
	}
	if sub.Reconciling {
		return state, errors.New("events: subscription is still reconciling")
	}
	if sub.State == "ACTIVE" && m.clock.Now().Before(m.renewAt) {
		return state, nil
	}
	m.mutationStarted = m.clock.Now()
	var op *api.Operation
	switch sub.State {
	case "ACTIVE":
		op, err = m.service.Subscriptions.Patch(sub.Name, &api.Subscription{Ttl: "0s"}).UpdateMask("ttl").Context(ctx).Do()
	case "SUSPENDED":
		op, err = m.service.Subscriptions.Reactivate(sub.Name, &api.ReactivateSubscriptionRequest{}).Context(ctx).Do()
	default:
		return state, fmt.Errorf("events: unsupported subscription state %q", sub.State)
	}
	if err != nil {
		return state, fmt.Errorf("events: reconcile %s: %w", sub.Name, err)
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
			return ledger.SubscriptionState{}, fmt.Errorf("events: poll %s: %w", op.Name, err)
		}
		op = next
		backoff = min(backoff*2, 10*time.Second)
	}
	m.pending = nil
	if op.Error != nil {
		if op.Error.Code == 6 {
			return m.adopt(ctx)
		} // google.rpc.Code.ALREADY_EXISTS
		return ledger.SubscriptionState{}, fmt.Errorf("events: operation failed (%d): %s", op.Error.Code, op.Error.Message)
	}
	var sub api.Subscription
	if err := json.Unmarshal(op.Response, &sub); err != nil {
		return ledger.SubscriptionState{}, fmt.Errorf("events: decode operation: %w", err)
	}
	return m.persist(&sub, m.mutationStarted)
}

func (m *Manager) validate(sub *api.Subscription) (time.Time, error) {
	if sub == nil || sub.Name == "" || sub.TargetResource != Target || sub.NotificationEndpoint == nil || sub.NotificationEndpoint.PubsubTopic != m.topic || sub.PayloadOptions == nil || !sub.PayloadOptions.IncludeResource {
		return time.Time{}, errors.New("events: subscription has missing or incompatible resource, topic or payload options")
	}
	for _, typ := range m.types {
		if !slices.Contains(sub.EventTypes, typ) {
			return time.Time{}, fmt.Errorf("events: subscription %s lacks event type %s", sub.Name, typ)
		}
	}
	expiry, err := time.Parse(time.RFC3339Nano, sub.ExpireTime)
	if err != nil {
		return time.Time{}, fmt.Errorf("events: invalid subscription expiry: %w", err)
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
	m.ledger.Subscription = state
	err = m.ledger.Save(m.path)
	if err != nil {
		m.ledger.Subscription = previous
	}
	m.ledgerMu.Unlock()
	if err != nil {
		return previous, fmt.Errorf("events: persist subscription: %w", err)
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
	}
	m.ambiguousUntil = time.Time{}
	return state, nil
}

// RenewLoop retries failures with exponential backoff up to two minutes. Long
// waits recheck wall time and the latest schedule at least once a minute, so
// clock jumps and lifecycle-driven renewal cannot leave a stale relative timer.
func (m *Manager) RenewLoop(ctx context.Context) {
	backoff := time.Second
	for ctx.Err() == nil {
		_, err := m.Reconcile(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			m.log.ErrorContext(ctx, "subscription reconciliation failed", "cause", err)
			if m.clock.Sleep(ctx, backoff) != nil {
				return
			}
			backoff = min(backoff*2, operationTimeout)
			continue
		}
		backoff = time.Second
		for ctx.Err() == nil {
			select {
			case m.gate <- struct{}{}:
			case <-ctx.Done():
				return
			}
			delay := m.renewAt.Sub(m.clock.Now())
			<-m.gate
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
// Returning errors lets the dispatcher nack/retry a transient failure.
func (m *Manager) HandleLifecycle(ctx context.Context, eventType string) error {
	switch eventType {
	case "google.workspace.events.subscription.v1.expirationReminder", "google.workspace.events.subscription.v1.expired", "google.workspace.events.subscription.v1.suspended":
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

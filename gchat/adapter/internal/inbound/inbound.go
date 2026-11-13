// Package inbound maps Chat changes to durable gc turns. Ambiguous deliveries
// are retained for operator resolution; they are never automatically replayed.
package inbound

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"gchat-adapter/internal/chat"
	"gchat-adapter/internal/config"
	"gchat-adapter/internal/gcclient"
	"gchat-adapter/internal/ledger"
	"gchat-adapter/internal/media"
	"gchat-adapter/internal/neversilent"
	"gchat-adapter/internal/principal"
	"gchat-adapter/internal/provenance"
	"gchat-adapter/internal/puller"
	"gchat-adapter/internal/wire"
	api "google.golang.org/api/chat/v1"
	"google.golang.org/api/googleapi"
)

type Options struct {
	NeverSilent *neversilent.Engine
	Config      *config.Config
	Ledger      *ledger.Ledger
	// LedgerMu must be the same mutex used by every other ledger consumer.
	LedgerMu       *sync.Mutex
	LedgerPath     string
	GC             *gcclient.Client
	Chat           *chat.Client
	IdentityUserID string
	// Principals writes who is speaking as each turn's first line; nil means
	// no owners, no organization, no directory: everyone is external until
	// the identity's own Workspace domain is learned from its own messages.
	Principals *principal.Resolver
	Logger     puller.Logger
	// Save optionally replaces Ledger.Save. It runs with LedgerMu held and must
	// durably save the supplied snapshot without retaining or modifying it.
	Save func(*ledger.Ledger) error
	// RetryDelay defaults to one second; retries back off exponentially. The
	// first four POSTs of a change wait 1, 2 and 4 times it inside the gate.
	RetryDelay time.Duration
	// SpaceJoined is told, outside the ledger lock, when the identity's own
	// membership in a space it did not know is observed. The app then sweeps
	// that space's history from the join time: Google's all-spaces subscription
	// does not deliver a newly joined space's messages at once (seen live).
	SpaceJoined func(space string)
	// PendingDeadline bounds receipt polling from the change's latest POST
	// (ChangeState.Posted; Created for a record without it). The default is
	// one hour, matching gc's receipt retention period.
	PendingDeadline time.Duration
	// Now supplies wall time for durable timestamps and pending deadlines.
	Now func() time.Time
	// RetryCap caps the wait between the POSTs that follow the fourth, when gc
	// keeps answering "not delivered". The default is DefaultRetryCap.
	RetryCap time.Duration
	// RetryBound ends every change in intent at its recorded time plus this
	// bound, whether it is retrying or held behind an earlier change of its
	// space. The default is DefaultRetryBound.
	RetryBound time.Duration
	// StuckAfter counts from the recorded time, or the latest POST if later
	// while pending, before a waiting mail. The default is DefaultStuckAfter.
	StuckAfter time.Duration
	// Raise tells the city's agent about one stuck or unresolved change. Poll
	// calls it at the end of its pass, outside the gate and the ledger lock,
	// at most RaisesPerPass times. It returns nil only once the report was
	// sent; a failure is tried again on a later pass, at most once a minute.
	// Nil disables raising.
	Raise func(context.Context, Raise) error
}

const (
	DefaultRetryCap   = time.Minute
	DefaultRetryBound = 24 * time.Hour
	DefaultStuckAfter = 2 * time.Minute
	// RaisesPerPass bounds the Raise calls of one poll pass.
	RaisesPerPass = 4
	// firstPOSTs run inside the gate with today's waits; later POSTs are
	// driven by Poll, one per call, no earlier than NextAttempt.
	firstPOSTs = 4
	// raiseRetry is the least time between two raise attempts of one change.
	raiseRetry = time.Minute
)

// Raise is one stuck or unresolved change for the city's agent. Body is the
// whole mail: it names the change and the commands for it, and never carries
// the message's text, the sender or gc's error text.
type Raise struct {
	// ID is the change ID that gc gchat resolve takes.
	ID      string
	Space   string
	Thread  string
	Message string
	Body    string
	// Ended marks a mail that says the message ended unresolved. It has its
	// own dedup key: a waiting mail of the same message may have reached gc
	// with its result lost, and gc drops a mail whose key is still live.
	Ended bool
	// Recorded is when the change's current life was recorded (its Created;
	// a replay starts a new life). The dedup key carries it.
	Recorded time.Time
}

// DedupKey is the mail's key at gc below the app's city and identity: one
// per life of the change and kind of mail. A retry of one mail keeps its key,
// so a send whose result was lost stays one mail at gc. A replay's life has
// keys of its own: gc drops a send whose key is still live, and a key of the
// earlier life must not drop a mail of the replay (gp-t542). The hash keeps
// the change ID out of the key.
func (r Raise) DedupKey() string {
	sum := sha256.Sum256([]byte(r.ID + "\x00" + r.Recorded.UTC().Format(time.RFC3339Nano)))
	key := fmt.Sprintf("delivery:%x", sum)
	if r.Ended {
		key += ":ended"
	}
	return key
}

type Processor struct {
	opts Options
	gate chan struct{}
	// wait is the in-gate wait of the first POSTs; tests replace it.
	wait func(context.Context, time.Duration) error
	// raiseMu serializes the raise phase of concurrent polls. raiseTried
	// throttles a change whose report failed; it is process memory only.
	raiseMu    sync.Mutex
	raiseTried map[string]time.Time
}

// ErrCredentials identifies a lookup held until Chat credentials or scopes are repaired.
var ErrCredentials = errors.New("inbound: credentials unavailable")

func New(opts Options) (*Processor, error) {
	if opts.Principals == nil {
		opts.Principals = principal.New(principal.Options{})
	}
	if opts.Config == nil || opts.Ledger == nil || opts.LedgerMu == nil || opts.GC == nil || opts.Chat == nil || (opts.Save == nil && opts.LedgerPath == "") {
		return nil, errors.New("inbound: config, clients, ledger, shared mutex and persistence are required")
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.RetryDelay == 0 {
		opts.RetryDelay = time.Second
	}
	if opts.RetryDelay < 0 {
		return nil, errors.New("inbound: retry delay must be positive")
	}
	if opts.PendingDeadline == 0 {
		opts.PendingDeadline = time.Hour
	}
	if opts.PendingDeadline < 0 {
		return nil, errors.New("inbound: pending deadline must be positive")
	}
	for _, d := range []struct {
		value    *time.Duration
		fallback time.Duration
		name     string
	}{{&opts.RetryCap, DefaultRetryCap, "retry cap"}, {&opts.RetryBound, DefaultRetryBound, "retry bound"}, {&opts.StuckAfter, DefaultStuckAfter, "stuck threshold"}} {
		if *d.value == 0 {
			*d.value = d.fallback
		}
		if *d.value < 0 {
			return nil, errors.New("inbound: " + d.name + " must be positive")
		}
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Save == nil {
		opts.Save = func(l *ledger.Ledger) error { return l.Save(opts.LedgerPath) }
	}
	return &Processor{opts: opts, gate: make(chan struct{}, 1), wait: wait, raiseTried: map[string]time.Time{}}, nil
}

// Handle is a puller.Handler. Pending receipts are durable recovery obligations,
// so they permit Ack; Poll must run regularly (normally every five seconds).
// Network requests never hold LedgerMu. Handle and Poll serialize their work,
// including live and catch-up callers, and waiting callers honor cancellation.
func (p *Processor) Handle(ctx context.Context, ev *puller.Event) (puller.Outcome, error) {
	return p.handle(ctx, ev, false)
}

// HandleChange feeds one catch-up change through the same validation, wake
// policy, dedup and delivery path as live events. Its durable turn includes a
// [catch-up] header; no synthetic transport ID is needed. As with Handle, Ack
// can include a durable pending receipt, whose resolution belongs to Poll.
func (p *Processor) HandleChange(ctx context.Context, c puller.Change) (puller.Outcome, error) {
	return p.handle(ctx, &puller.Event{Changes: []puller.Change{c}}, true)
}

func (p *Processor) handle(ctx context.Context, ev *puller.Event, catchUp bool) (puller.Outcome, error) {
	if err := p.enter(ctx); err != nil {
		return puller.Nack, err
	}
	defer p.leave()
	if ev == nil {
		return puller.Quarantine, errors.New("inbound: missing event")
	}
	l, err := p.snapshot()
	if err != nil {
		return puller.Nack, err
	}
	if (ev.PubSubID != "" && l.Transport.Seen("pubsub:"+ev.PubSubID)) || (ev.CloudEventID != "" && l.Transport.Seen("cloudevent:"+ev.CloudEventID)) {
		return puller.Ack, nil
	}
	// Validate the whole batch before accepting any changes. The puller preserves
	// the raw event when Quarantine is returned.
	for _, c := range ev.Changes {
		if err := validate(c); err != nil {
			return puller.Quarantine, err
		}
	}
	if len(ev.Changes) > 0 && p.identity() == "" {
		return puller.Nack, errors.New("inbound: identity user ID unavailable")
	}
	for _, c := range ev.Changes {
		if err := ctx.Err(); err != nil {
			return puller.Nack, err
		}
		if err := p.change(ctx, c, catchUp); err != nil {
			return puller.Nack, err
		}
	}
	if ev.PubSubID == "" && ev.CloudEventID == "" {
		return puller.Ack, nil
	}
	err = p.update(func(l *ledger.Ledger) {
		if ev.PubSubID != "" {
			l.Transport.Add("pubsub:" + ev.PubSubID)
		}
		if ev.CloudEventID != "" {
			l.Transport.Add("cloudevent:" + ev.CloudEventID)
		}
	})
	if err != nil {
		return puller.Nack, err
	}
	return puller.Ack, nil
}

// Poll performs one recovery pass. The app calls it every five seconds, also
// after startup. Concluded receipts are honored even past the pending deadline;
// lookup failures past that deadline get one persisted grace pass. Changes are
// taken oldest first, so the changes of one space follow each other in one
// pass once the first lands. After the pass, outside the gate, Poll raises
// what is due (see Options.Raise); a raise failure is logged, not returned.
func (p *Processor) Poll(ctx context.Context) error {
	err := p.Pass(ctx)
	p.raise(ctx)
	return err
}

// Pass is Poll without the raise phase, for a caller that answers a request
// (resolve --replay): a mail must not hold that answer. The app's poll loop
// raises on its next pass, within seconds.
func (p *Processor) Pass(ctx context.Context) error {
	if err := p.enter(ctx); err != nil {
		return err
	}
	defer p.leave()
	l, err := p.snapshot()
	if err != nil {
		return err
	}
	if err := ReconcileNeverSilent(l, p.opts.NeverSilent); err != nil {
		return err
	}
	ids := make([]string, 0)
	for id, c := range l.Changes {
		if c.Status == "pending" || c.Status == "intent" {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return earlier(l.Changes[ids[i]], l.Changes[ids[j]]) })
	var failures []error
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return err
		}
		c := l.Changes[id]
		if c.Status == "intent" {
			if err := p.resume(ctx, id); err != nil {
				failures = append(failures, err)
			}
			continue
		}
		r, err := p.opts.GC.Receipt(ctx, c.ReceiptID)
		if err != nil {
			failures = append(failures, errors.New("inbound: receipt lookup failed"))
			if ctx.Err() == nil {
				late := !p.opts.Now().Before(sentAt(c).Add(p.opts.PendingDeadline))
				var saveErr error
				if late && c.Reason == receiptLookupGrace {
					saveErr = p.unresolved(id, "receipt lookup failed beyond deadline")
				} else if late || c.Reason == receiptLookupGrace {
					// Reason is durable while pending; Attempts counts POSTs only.
					// A pre-deadline pass breaks the streak after a clock correction.
					saveErr = p.update(func(l *ledger.Ledger) {
						if current := l.Changes[id]; current != nil && current.Status == "pending" {
							current.Reason = ""
							if late {
								current.Reason = receiptLookupGrace
							}
						}
					})
				}
				if saveErr != nil {
					failures = append(failures, saveErr)
				}
			}
			continue
		}
		switch r.State {
		case "pending":
			if !p.opts.Now().Before(sentAt(c).Add(p.opts.PendingDeadline)) {
				err = p.unresolved(id, "receipt pending beyond deadline")
			} else if c.Reason == receiptLookupGrace {
				err = p.update(func(l *ledger.Ledger) {
					if current := l.Changes[id]; current != nil && current.Status == "pending" {
						current.Reason = ""
					}
				})
			}
		case "concluded":
			if r.Delivery.Status == "pending" {
				err = p.concludedPending(id, r.Delivery)
			} else {
				err = p.outcome(id, r.Delivery)
				if err == nil {
					err = p.resume(ctx, id)
				}
			}
		default:
			err = p.unresolved(id, "receipt unavailable or unknown")
		}
		if err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

const receiptLookupGrace = "receipt lookup failed beyond deadline; retry pending"

// Durable reasons this file judges by, beside the many it only reports.
const (
	reasonRetry       = "retry"
	reasonWithdrawn   = "message deleted or superseded before retry"
	reasonExhausted   = "delivery retries exhausted"
	reasonRetryBound  = "not delivered within the retry bound"
	reasonHeld        = "gc holds the delivery; operator attention"
	reasonUnconcluded = "receipt concluded with pending delivery"
	reasonLanded      = "landed_unconfirmed"
)

// The closed set of ChangeState.Cause. A waiting change has one of the first
// four; an unresolved change keeps its waiting word when a bound ended it.
const (
	causeDraftDeferred      = "draft_deferred"
	causePendingInteraction = "pending_interaction"
	causeNotDelivered       = "not_delivered"
	causeBehindEarlier      = "behind_earlier"
	causeHeld               = "held"
	causeWithdrawn          = "withdrawn"
	causeUnknown            = "unknown"
)

// earlier is the order of changes: by recorded time, then by ID.
func earlier(a, b *ledger.ChangeState) bool {
	if a.Created.Equal(b.Created) {
		return a.ID < b.ID
	}
	return a.Created.Before(b.Created)
}

// sentAt is when the latest POST was made; a record written before Posted
// existed counts from when it was recorded.
func sentAt(c *ledger.ChangeState) time.Time {
	if c.Posted.IsZero() {
		return c.Created
	}
	return c.Posted
}

// heldBack reports whether an earlier change of c's space is in intent: a
// later message never overtakes one that is still retrying or not yet sent.
// A pending change holds nothing (gp-dl26, ruling B). Holding behind it spaced
// a busy agent's burst one receipt apart and mailed false alarms. The cost,
// accepted: a later message can reach the agent before an earlier one whose
// receipt then concludes "failed" and is retried, as adapter 0.5.0 allows.
func heldBack(l *ledger.Ledger, c *ledger.ChangeState) bool {
	for _, other := range l.Changes {
		if other.Space == c.Space && other.ID != c.ID && other.Status == "intent" && earlier(other, c) {
			return true
		}
	}
	return false
}

// retryWait continues the in-gate doubling after the first POSTs:
// RetryDelay * 2^(attempts-1), capped at RetryCap before it can overflow.
func (p *Processor) retryWait(attempts int) time.Duration {
	d := p.opts.RetryDelay
	for i := 1; i < attempts; i++ {
		if d >= p.opts.RetryCap-d {
			return p.opts.RetryCap
		}
		d *= 2
	}
	return min(d, p.opts.RetryCap)
}

// waitCause names why gc did not deliver, from its status and error texts.
// The texts are read here and never stored or logged.
func waitCause(d wire.Delivery) string {
	if d.Status == "held" {
		return causeDraftDeferred
	}
	cause := causeNotDelivered
	for _, m := range d.Members {
		if strings.Contains(m.Error, "preserved draft deferred delivery") {
			return causeDraftDeferred
		}
		if strings.Contains(m.Error, "pending interaction") {
			cause = causePendingInteraction
		}
	}
	return cause
}

// unresolvedCause is the cause a change carries once it is unresolved.
func unresolvedCause(reason, waiting string) string {
	switch reason {
	case reasonWithdrawn:
		return causeWithdrawn
	case reasonHeld:
		return causeHeld
	case reasonRetryBound, reasonExhausted:
		if waiting != "" {
			return waiting
		}
	}
	// Includes a change that met the bound without any answer from gc (gc
	// down or restarting the whole time): nothing says it was not delivered.
	return causeUnknown
}

// concludedPending judges a receipt that concluded with the status pending.
// A terminal hold is for an operator. A paste every member took whole, with
// no terminal hold, landed: gc vouches the whole message is in the terminal,
// not that the agent read it. Anything else stays unresolved.
func (p *Processor) concludedPending(id string, d wire.Delivery) error {
	if err := p.opts.NeverSilent.Result(id, "pending", deliverySessions(d)); err != nil {
		return err
	}
	terminal := d.Terminal
	for _, m := range d.Members {
		terminal = terminal || m.Terminal
	}
	if terminal {
		if noticeWholePaste(d) {
			at := p.opts.Now()
			if err := p.update(func(l *ledger.Ledger) {
				if c := l.Changes[id]; c != nil {
					c.NoticeDelivered = true
					markUnresolved(l, id, c, reasonHeld, at)
				}
			}); err != nil {
				return err
			}
			return p.opts.NeverSilent.ResultAt(id, "delivered", deliverySessions(d), at)
		}
		return p.unresolved(id, reasonHeld)
	}
	if !landedWhole(d) {
		return p.unresolved(id, reasonUnconcluded)
	}
	if err := p.opts.NeverSilent.Result(id, "delivered", deliverySessions(d)); err != nil {
		return err
	}
	committed := false
	err := p.update(func(l *ledger.Ledger) {
		c := l.Changes[id]
		if c == nil || c.Status != "pending" {
			return
		}
		l.Counters["turns:"+c.Space]++
		l.Counters["unconfirmed:"+c.Space]++
		c.Status, c.Reason, c.Cause = "committed", reasonLanded, ""
		c.NoticeAt = p.opts.Now()
		committed = true
	})
	if err == nil && committed {
		p.opts.Logger.Info("inbound delivery landed unconfirmed", "change", id)
	}
	return err
}

func landedWhole(d wire.Delivery) bool {
	for _, m := range d.Members {
		if m.Terminal {
			return false
		}
	}
	return noticeWholePaste(d)
}

func noticeWholePaste(d wire.Delivery) bool {
	for _, m := range d.Members {
		if m.Status != "delivered" && (m.Status != "pending" || m.ExpectedBytes <= 0 || m.DeliveredBytes != m.ExpectedBytes) {
			return false
		}
	}
	return len(d.Members) > 0
}

func (p *Processor) enter(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case p.gate <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (p *Processor) leave() { <-p.gate }
func (p *Processor) identity() string {
	if p.opts.IdentityUserID != "" {
		return p.opts.IdentityUserID
	}
	return p.opts.Chat.IdentityUserID()
}

// Copy-on-save keeps a failed write from making an in-memory terminal record
// look durable. Assignment preserves the shared ledger pointer used by the app.
func (p *Processor) update(fn func(*ledger.Ledger)) error {
	p.opts.LedgerMu.Lock()
	defer p.opts.LedgerMu.Unlock()
	l, err := copyLedger(p.opts.Ledger)
	if err != nil {
		return err
	}
	fn(l)
	if err := p.opts.Save(l); err != nil {
		return errors.New("inbound: ledger save failed")
	}
	*p.opts.Ledger = *l
	return nil
}
func (p *Processor) snapshot() (*ledger.Ledger, error) {
	p.opts.LedgerMu.Lock()
	defer p.opts.LedgerMu.Unlock()
	return copyLedger(p.opts.Ledger)
}
func copyLedger(l *ledger.Ledger) (*ledger.Ledger, error) {
	return l.Clone(), nil
}

func validate(c puller.Change) error {
	var name, resource string
	switch c.Kind {
	case "message.created", "message.updated", "message.deleted":
		resource = "messages"
		if c.Message != nil {
			name = c.Message.Name
		}
		if c.Kind != "message.deleted" {
			if c.Message == nil || c.Message.Sender == nil || c.Message.Sender.Name == "" {
				return errors.New("inbound: message sender missing")
			}
			if c.Kind == "message.updated" && c.Message.LastUpdateTime == "" {
				return errors.New("inbound: update time missing")
			}
			for _, value := range []string{c.Message.CreateTime, c.Message.LastUpdateTime} {
				if value != "" {
					if _, err := time.Parse(time.RFC3339Nano, value); err != nil {
						return errors.New("inbound: invalid message timestamp")
					}
				}
			}
		}
	case "reaction.created", "reaction.deleted":
		resource = "reactions"
		if c.Reaction != nil {
			name = c.Reaction.Name
		}
	case "membership.created", "membership.updated", "membership.deleted":
		resource = "members"
		if c.Membership != nil {
			name = c.Membership.Name
		}
	default:
		return errors.New("inbound: unsupported change kind")
	}
	parts := strings.Split(name, "/")
	want := 4
	if resource == "reactions" {
		want = 6
	}
	if len(parts) != want || parts[0] != "spaces" || strings.Join(parts[:2], "/") != c.Space {
		return errors.New("inbound: invalid resource name or space")
	}
	if (resource == "reactions" && (parts[2] != "messages" || parts[4] != resource)) || (resource != "reactions" && parts[2] != resource) {
		return errors.New("inbound: invalid resource kind")
	}
	for _, part := range parts {
		if part == "" || part == "." || part == ".." || strings.ContainsAny(part, "\\\x00") {
			return errors.New("inbound: invalid resource segment")
		}
	}
	return nil
}

func (p *Processor) change(ctx context.Context, c puller.Change, catchUp bool) error {
	switch {
	case strings.HasPrefix(c.Kind, "membership."):
		m := c.Membership
		if c.Kind == "membership.deleted" || m.Member == nil || m.Member.Name != p.identity() || (m.State != "" && m.State != "JOINED") {
			return nil
		}
		// History before the join is not the identity's to read into the
		// city: both cursors start at the join time (now, if Google omits it).
		joined := p.opts.Now().UTC()
		if t, err := time.Parse(time.RFC3339Nano, m.CreateTime); err == nil {
			joined = t.UTC()
		}
		added := false
		err := p.update(func(l *ledger.Ledger) {
			if l.Spaces[c.Space] == nil {
				l.Spaces[c.Space] = &ledger.SpaceState{Wake: string(p.opts.Config.Wake(c.Space)), JoinedAt: joined, MessageCursor: joined, EventCursor: joined}
				added = true
			}
		})
		if err == nil && added && p.opts.SpaceJoined != nil {
			p.opts.SpaceJoined(c.Space)
		}
		return err
	case strings.HasPrefix(c.Kind, "reaction."):
		return p.reaction(ctx, c, catchUp)
	default:
		return p.message(ctx, c, catchUp)
	}
}

var errSpaceUnavailable = errors.New("inbound: space type unavailable")

func unavailableSpace(err error) bool {
	// A not-a-member or resource-level 403 is unavailable; a 401 or a scope
	// 403 holds for credential repair.
	kind, _ := chat.Classify(err)
	return errors.Is(err, errSpaceUnavailable) || kind == "not_found" || chat.ResourceDenied(err) || permanentLookupFailure(err)
}

func unavailableParent(err error) bool {
	// A not-a-member or resource-level 403 proves the parent inaccessible; a
	// scope 403 holds for credential repair just like 401.
	kind, _ := chat.Classify(err)
	return kind == "not_found" || chat.ResourceDenied(err) || permanentLookupFailure(err)
}

func permanentLookupFailure(err error) bool {
	kind, _ := chat.Classify(err)
	return kind == "permanent"
}

// lookupError exposes a safe classifier and status, never Google's response body.
func lookupError(lookup string, err error) error {
	kind, _ := chat.Classify(err)
	cause := classifiedLookupError{cause: err}
	if kind == "auth" && !chat.ResourceDenied(err) {
		return fmt.Errorf("inbound: %s lookup failed: %w: %w", lookup, cause, ErrCredentials)
	}
	return fmt.Errorf("inbound: %s lookup failed: %w", lookup, cause)
}

// Keep the original chain available to errors.Is/As without formatting its body.
type classifiedLookupError struct{ cause error }

func (e classifiedLookupError) Error() string { return lookupCause(e.cause) }
func (e classifiedLookupError) Unwrap() error { return e.cause }

func lookupCause(err error) string {
	kind, _ := chat.Classify(err)
	var apiErr *googleapi.Error
	if errors.As(err, &apiErr) {
		return fmt.Sprintf("%s (HTTP %d)", kind, apiErr.Code)
	}
	return kind
}

func retainedReason(reason string, err error) string {
	if kind, _ := chat.Classify(err); kind == "permanent" {
		return reason + ": " + lookupCause(err)
	}
	return reason
}

func (p *Processor) space(ctx context.Context, name string, supplied *api.Space) (*ledger.SpaceState, error) {
	l, err := p.snapshot()
	if err != nil {
		return nil, err
	}
	s := l.Spaces[name]
	if s == nil {
		s = &ledger.SpaceState{}
	}
	if s.Wake == "" {
		s.Wake = string(p.opts.Config.Wake(name))
	}
	if s.Kind != "" {
		return s, nil
	}
	kind := ""
	if supplied != nil {
		kind = spaceKind(supplied)
	}
	if kind == "" {
		spaces, err := p.opts.Chat.ListSpaces(ctx)
		if err != nil {
			return nil, err // callers classify it and return only a safe summary
		}
		for _, candidate := range spaces {
			if candidate.Name == name {
				kind = spaceKind(&candidate)
				break
			}
		}
	}
	if kind == "" {
		return nil, errSpaceUnavailable
	}
	s.Kind = kind
	return s, nil
}
func spaceKind(s *api.Space) string {
	if s.SpaceType == "DIRECT_MESSAGE" || s.Type == "DM" {
		return "dm"
	}
	if s.SpaceType == "SPACE" || s.SpaceType == "GROUP_CHAT" || s.Type == "ROOM" {
		return "room"
	}
	return ""
}

func (p *Processor) message(ctx context.Context, c puller.Change, catchUp bool) error {
	m := c.Message
	id := m.Name
	l, err := p.snapshot()
	if err != nil {
		return err
	}
	previous := l.Messages[m.Name]
	base := l.Changes[m.Name]
	revision := m.CreateTime
	if m.LastUpdateTime != "" {
		revision = m.LastUpdateTime
	}
	updated, _ := time.Parse(time.RFC3339Nano, revision)
	if c.Kind == "message.deleted" {
		id += "#deleted"
	} else if c.Kind == "message.updated" && base != nil {
		id += "#updated#" + m.LastUpdateTime
		// An update received before creation owns the base ID, including when
		// a redelivery resumes its interrupted or transiently failed POST.
		if base.Kind == "message.updated" && previous != nil && previous.Revision.Equal(updated) {
			id = m.Name
		}
	}
	if l.Changes[id] != nil {
		return p.resume(ctx, id)
	}
	if previous != nil && (previous.Deleted || (base != nil && c.Kind != "message.deleted" && (c.Kind == "message.created" || !updated.After(previous.Revision)))) {
		return nil
	}
	state := ledger.MessageState{Revision: updated}
	if previous != nil {
		state = *previous
		if updated.After(state.Revision) {
			state.Revision = updated
		}
	}
	if m.Thread != nil {
		state.Thread = m.Thread.Name
	}
	if m.Sender != nil {
		state.Mine = m.Sender.Name == p.identity()
	}
	if c.Kind == "message.deleted" {
		state.Deleted = true
		for changeID, change := range l.Changes {
			if change.Message == m.Name {
				if err := p.opts.NeverSilent.Forget(changeID); err != nil {
					return err
				}
			}
		}
	}
	// The identity's own messages tell which Workspace domain is the
	// organization; a sender in it is a colleague even without an address.
	learnedDomain := ""
	if state.Mine && m.Sender != nil && m.Sender.DomainId != "" && m.Sender.DomainId != l.IdentityDomainID {
		learnedDomain = m.Sender.DomainId
	}
	speaker := p.opts.Principals.Speaker(ctx, m.Sender, l.IdentityDomainID)
	// The message's own name is the item the key holder can verify at the
	// source when the agent acts on it (boxboxbox-google mail --for).
	speaker.Item = provenance.ChatItem(m.Name)
	// Deleting a never-observed message still needs a tombstone, but no turn.
	if c.Kind == "message.deleted" && previous == nil {
		return p.record(c, id, wire.InboundMessage{}, &state, nil, "local", catchUp, learnedDomain)
	}
	forwarded := false
	for _, change := range l.Changes {
		if change.Message == m.Name && strings.HasPrefix(change.Kind, "message.") && change.Reason != "local" && change.Reason != "own" && change.Reason != "wake_policy" && !strings.HasPrefix(change.Reason, "space unavailable") {
			forwarded = true
			break
		}
	}
	local := ""
	if state.Mine {
		local = "own"
	} else if c.Kind == "message.deleted" && !forwarded {
		local = "local"
	}
	s, err := p.space(ctx, c.Space, m.Space)
	if err != nil {
		if unavailableSpace(err) {
			w := p.envelope(id, c.Space, "", m.Sender, state.Thread)
			w.Text = provenance.Render(speaker, renderMentions(m))
			reason := local
			if reason == "" {
				reason = retainedReason("space unavailable", err)
			}
			return p.record(c, id, w, &state, nil, reason, catchUp, learnedDomain)
		}
		return lookupError("space", err)
	}
	w := p.envelope(id, c.Space, s.Kind, m.Sender, state.Thread)
	w.Text = renderMentions(m)
	framed := false
	if c.Kind == "message.deleted" && !state.Mine {
		w.Text = "deleted"
		// Google reports a deletion without its sender; the original turn
		// carries both the actor and the frame.
		if base != nil {
			var original wire.InboundMessage
			if json.Unmarshal([]byte(base.Event), &original) == nil {
				w.Actor = original.Actor
				if line, ok := provenance.FrameOf(original.Text); base.ProvenanceFramed && ok {
					w.Text, framed = line+"\ndeleted", true
				}
			}
		}
	} else if local == "" && !forwarded && s.Kind != "dm" && s.Wake == "mentions" && !mentions(m, p.identity()) {
		local = "wake_policy"
	}
	if local == "" && c.Kind != "message.deleted" {
		w.Text, w.Attachments = NormalizeContent(ctx, p.opts.Chat, p.opts.Config.Endpoints, m)
	}
	if c.Kind == "message.updated" && id != m.Name {
		w.Text = "edited: " + w.Text
	}
	if !framed {
		w.Text = provenance.Render(speaker, w.Text)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if local == "" && c.Kind == "message.created" && !catchUp && m.Sender != nil && m.Sender.Type == "HUMAN" && (s.Kind == "dm" || mentions(m, p.identity())) {
		if err := p.opts.NeverSilent.Track(neversilent.Message{ID: id, Conversation: c.Space, Thread: state.Thread, Created: p.opts.Now()}); err != nil {
			return err
		}
	}
	if err := p.record(c, id, w, &state, s, local, catchUp, learnedDomain); err != nil {
		return err
	}
	return p.resume(ctx, id)
}

func (p *Processor) reaction(ctx context.Context, c puller.Change, catchUp bool) error {
	r := c.Reaction
	action := strings.TrimPrefix(c.Kind, "reaction.")
	id := r.Name + "#" + action
	l, err := p.snapshot()
	if err != nil {
		return err
	}
	if l.Changes[id] != nil {
		return p.resume(ctx, id)
	}
	parent := strings.Split(r.Name, "/reactions/")[0]
	speaker := p.opts.Principals.Speaker(ctx, r.User, l.IdentityDomainID)
	state := l.Messages[parent]
	if state == nil {
		m, err := p.opts.Chat.GetMessage(ctx, parent)
		if err != nil {
			if unavailableParent(err) {
				w := p.envelope(id, c.Space, "", r.User, "")
				w.Text = provenance.Render(speaker, reactionText(r, action, parent))
				return p.record(c, id, w, nil, nil, retainedReason("parent unavailable", err), catchUp, "")
			}
			return lookupError("reaction parent", err)
		}
		state = &ledger.MessageState{Mine: m.Sender != nil && m.Sender.Name == p.identity()}
		if m.Thread != nil {
			state.Thread = m.Thread.Name
		}
	}
	s, err := p.space(ctx, c.Space, nil)
	if err != nil {
		if unavailableSpace(err) {
			w := p.envelope(id, c.Space, "", r.User, state.Thread)
			w.Text = provenance.Render(speaker, reactionText(r, action, parent))
			reason := "local"
			if state.Mine {
				reason = retainedReason("space unavailable", err)
			}
			return p.record(c, id, w, state, nil, reason, catchUp, "")
		}
		return lookupError("space", err)
	}
	w := p.envelope(id, c.Space, s.Kind, r.User, state.Thread)
	w.Text = provenance.Render(speaker, reactionText(r, action, parent))
	if action == "deleted" && r.User == nil {
		if created := l.Changes[r.Name+"#created"]; created != nil {
			var original wire.InboundMessage
			if json.Unmarshal([]byte(created.Event), &original) == nil {
				w.Actor = original.Actor
				if created.ProvenanceFramed {
					w.Text = strings.Replace(strings.TrimPrefix(original.Text, "[catch-up]\n"), "reaction created:", "reaction deleted:", 1)
				}
			}
		}
	}
	local := ""
	if !state.Mine {
		local = "local"
	}
	if err := p.record(c, id, w, state, s, local, catchUp, ""); err != nil {
		return err
	}
	return p.resume(ctx, id)
}

func reactionText(r *api.Reaction, action, parent string) string {
	emoji := "reaction"
	if r.Emoji != nil {
		if r.Emoji.Unicode != "" {
			emoji = r.Emoji.Unicode
		} else if r.Emoji.CustomEmoji != nil {
			emoji = r.Emoji.CustomEmoji.Uid
		}
	}
	return "reaction " + action + ": " + emoji + " on " + parent
}

func (p *Processor) envelope(id, space, kind string, user *api.User, thread string) wire.InboundMessage {
	a := wire.Actor{}
	if user != nil {
		a = wire.Actor{ID: user.Name, DisplayName: user.DisplayName, IsBot: user.Type == "BOT"}
		if a.DisplayName == "" {
			a.DisplayName = a.ID
		}
	}
	return wire.InboundMessage{ProviderMessageID: id, Conversation: wire.Conversation{ScopeID: p.opts.Config.Secrets.CityName, Provider: "gchat", AccountID: p.opts.Config.Identity().Name, ConversationID: space, Kind: kind},
		Actor: a, ReplyToMessageID: thread, ReceivedAt: p.opts.Now().UTC()}
}

// record persists a change and fills an empty space Wake from config. Once
// persisted, later config default changes no longer reach that space; a spaces
// reset must clear or replace its stored Wake to inherit a new default.
func (p *Processor) record(c puller.Change, id string, w wire.InboundMessage, state *ledger.MessageState, space *ledger.SpaceState, local string, catchUp bool, learnedDomain string) error {
	if catchUp {
		w.Text = "[catch-up]\n" + w.Text
	}
	data, err := json.Marshal(w)
	if err != nil {
		return errors.New("inbound: encode turn failed")
	}
	name := ""
	if c.Message != nil {
		name = c.Message.Name
	}
	if c.Reaction != nil {
		name = strings.Split(c.Reaction.Name, "/reactions/")[0]
	}
	unavailable := strings.HasPrefix(local, "space unavailable") || strings.HasPrefix(local, "parent unavailable")
	err = p.update(func(l *ledger.Ledger) {
		status := "intent"
		if local != "" {
			status = "committed"
		}
		l.Changes[id] = &ledger.ChangeState{ID: id, Space: c.Space, Message: name, Kind: c.Kind, Status: status, Created: p.opts.Now().UTC(), Event: string(data), Reason: local, CatchUp: catchUp}
		l.Changes[id].ProvenanceFramed = w.ProviderMessageID != ""
		if learnedDomain != "" {
			l.IdentityDomainID = learnedDomain
		}
		if unavailable {
			l.Counters["unavailable:"+c.Space]++
		}
		if state != nil {
			// Outbound can record its send key while a Chat lookup is in flight.
			// That field belongs to outbound; retain its latest value under the
			// shared mutex instead of overwriting it with our earlier snapshot.
			if current := l.Messages[name]; current != nil {
				state.SendKey = current.SendKey
			}
			l.Messages[name] = state
		}
		if space != nil {
			// Preserve cursors/backlog and any concurrent wake-policy update.
			if current := l.Spaces[c.Space]; current != nil {
				current.Kind = space.Kind
				if current.Wake == "" {
					current.Wake = space.Wake
				}
			} else {
				l.Spaces[c.Space] = space
			}
		}
	})
	if err == nil && unavailable {
		p.opts.Logger.Warn("inbound change retained", "space", c.Space, "change", id, "message", name, "cause", local)
	}
	return err
}

// resume POSTs a change in intent. The first four POSTs run here, inside the
// gate, with waits of 1, 2 and 4 times RetryDelay. After the fourth, a change
// gc keeps answering "not delivered" stays in intent with NextAttempt set, and
// each call makes at most one POST, none before NextAttempt: Poll drives it,
// and a Pub/Sub redelivery takes the same path. A change never overtakes an
// earlier change of its space that is in intent (heldBack) and ends at
// RetryBound.
func (p *Processor) resume(ctx context.Context, id string) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		l, err := p.snapshot()
		if err != nil {
			return err
		}
		c := l.Changes[id]
		if c == nil || c.Status != "intent" {
			return nil
		}
		if superseded(l, c) {
			return p.unresolved(id, reasonWithdrawn)
		}
		if c.Attempts > 0 && c.Reason != reasonRetry {
			return p.unresolved(id, "interrupted POST; delivery unknown")
		}
		if c.Reason == reasonRetry && c.Attempts >= firstPOSTs && c.Cause == "" {
			// Written by adapter 0.5.0 between the save that put a fourth "not
			// delivered" back in intent and the save that ended it (gp-dl26,
			// ruling C). This code never writes it: every retry carries a
			// cause. End it as 0.5.0 would have, never POST it again.
			return p.unresolved(id, reasonExhausted)
		}
		now := p.opts.Now()
		if !now.Before(c.Created.Add(p.opts.RetryBound)) {
			return p.unresolved(id, reasonRetryBound)
		}
		if heldBack(l, c) {
			if c.Cause == causeBehindEarlier {
				return nil
			}
			return p.update(func(l *ledger.Ledger) {
				if current := l.Changes[id]; current != nil && current.Status == "intent" {
					current.Cause = causeBehindEarlier
				}
			})
		}
		later := c.Attempts >= firstPOSTs
		// No wait is longer than RetryCap: after a wall clock was set back, a
		// next attempt further away than that is due (a retry after "not
		// delivered" is clean).
		if later && now.Before(c.NextAttempt) && c.NextAttempt.Sub(now) <= p.opts.RetryCap {
			return nil
		}
		if !later && c.Attempts > 0 {
			if err := p.wait(ctx, p.opts.RetryDelay*time.Duration(1<<uint(c.Attempts-1))); err != nil {
				return err
			}
		}
		var w wire.InboundMessage
		if json.Unmarshal([]byte(c.Event), &w) != nil || w.ProviderMessageID != id {
			return p.unresolved(id, "stored turn is malformed")
		}
		// Persist the attempt before crossing the process boundary. A crash or
		// lost response after this point is ambiguous, never safe to repost.
		attempted := false
		if err := p.update(func(l *ledger.Ledger) {
			if c := l.Changes[id]; c != nil {
				c.Attempts++
				c.Reason = ""
				c.Posted = p.opts.Now().UTC()
				// This POST uses the next attempt; a later "not delivered" sets a new one.
				c.NextAttempt = time.Time{}
				attempted = true
			}
		}); err != nil {
			return err
		}
		if !attempted {
			return nil
		}
		r, err := p.opts.GC.Inbound(ctx, w)
		if err != nil {
			var responseErr *gcclient.HTTPError
			answered := errors.As(err, &responseErr)
			var op *net.OpError
			if (answered && (responseErr.StatusCode >= 500 || responseErr.StatusCode == 429 || responseErr.StatusCode == 408 || stoppedCity(responseErr))) || (errors.As(err, &op) && op.Op == "dial") {
				kind := "other"
				if (answered && (responseErr.StatusCode >= 500 || stoppedCity(responseErr))) || (op != nil && op.Op == "dial") {
					kind = "unreachable"
				}
				if noticeErr := p.opts.NeverSilent.Result(id, kind, nil); noticeErr != nil {
					return noticeErr
				}
				if err := p.update(func(l *ledger.Ledger) {
					if c := l.Changes[id]; c != nil {
						c.Attempts--
						c.Reason = "retry"
					}
				}); err != nil {
					return err
				}
				return errors.New("inbound: gc temporarily unavailable; retry POST")
			}
			if answered && responseErr.StatusCode >= 400 && responseErr.StatusCode < 500 {
				// The status and machine-readable code describe a definite rejection.
				// Every 404 except the stopped-city response above is definite.
				// Never log HTTPError itself: it includes the response body.
				reason := fmt.Sprintf("gc rejected the POST: HTTP %d", responseErr.StatusCode)
				if code := reasonCode(responseErr.Code()); code != "" {
					reason += " " + code
				}
				return p.unresolved(id, reason)
			}
			if answered && responseErr.StatusCode >= 300 && responseErr.StatusCode < 400 {
				return p.unresolved(id, fmt.Sprintf("gc answered HTTP %d; delivery unknown", responseErr.StatusCode))
			}
			if err := p.unresolved(id, "POST response unavailable; delivery unknown"); err != nil {
				return err
			}
			return ctx.Err()
		}
		if err := p.outcome(id, r.Delivery); err != nil {
			return err
		}
		if later {
			return nil
		}
	}
}

func stoppedCity(err *gcclient.HTTPError) bool {
	return err.StatusCode == 404 && (err.Code() == "city-not-found" ||
		(err.Problem != nil && strings.HasPrefix(err.Problem.Detail, "not_found: city not found or not running")))
}

// reasonCode admits only short ASCII tokens into durable reasons and logs.
func reasonCode(code string) string {
	for _, c := range code {
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return ""
		}
	}
	if len(code) > 64 {
		return code[:64]
	}
	return code
}

// An unsent or explicitly failed old revision must not resurrect content after
// a newer edit or tombstone. Keep it for an operator instead of replaying it.
// Pending receipts still get polled: they describe a POST already in flight.
func superseded(l *ledger.Ledger, c *ledger.ChangeState) bool {
	if c.Kind != "message.created" && c.Kind != "message.updated" {
		return false
	}
	state := l.Messages[c.Message]
	if state == nil {
		return false
	}
	if state.Deleted {
		return true
	}
	if stamp, ok := strings.CutPrefix(c.ID, c.Message+"#updated#"); ok {
		revision, err := time.Parse(time.RFC3339Nano, stamp)
		return err == nil && state.Revision.After(revision)
	}
	for _, other := range l.Changes {
		if other.Message == c.Message && other.Kind == "message.updated" && other.ID != c.ID {
			return true
		}
	}
	return false
}

// outcome applies gc's answer. "failed" and "held" are a definite "not
// delivered": nothing reached the terminal, so the change goes back to intent
// and is retried until it lands or a bound ends it, never by count. "partial"
// put a fragment in the terminal and each retry can add another, so it keeps
// the four-POST bound: no POST after the fourth, nor after a fragment answer
// that comes later. So does "held" with a "partial" member: gc ranks held
// above partial when a fan-out has both (gp-dl26, ruling F). The change keeps
// the mark (Fragment), so the bound holds when a later answer is "failed",
// across a restart too (gp-t542).
func (p *Processor) outcome(id string, d wire.Delivery) error {
	status := d.Status
	noticeKind := status
	if status == "held" && !fragment(d) {
		noticeKind = "failed"
	}
	if err := p.opts.NeverSilent.Result(id, noticeKind, deliverySessions(d)); err != nil {
		return err
	}
	if status == "pending" && d.ReceiptID == "" {
		return p.unresolved(id, "pending delivery has no receipt")
	}
	switch status {
	case "delivered", "no_route", "pending", "failed", "held", "partial":
	default:
		return p.unresolved(id, "unrecognized delivery status")
	}
	exhausted := false
	err := p.update(func(l *ledger.Ledger) {
		c := l.Changes[id]
		if c == nil {
			return
		}
		if d.ReceiptID != "" {
			c.ReceiptID = d.ReceiptID
		}
		c.NoticeAt = p.opts.Now()
		switch status {
		case "delivered", "no_route":
			if c.Status != "committed" {
				if status == "no_route" {
					l.Counters["no_route:"+c.Space]++
				} else {
					l.Counters["turns:"+c.Space]++
				}
			}
			c.Status, c.Reason, c.Cause = "committed", status, ""
		case "pending":
			// gc has the message in hand; the cause of an earlier wait is past.
			c.Status, c.Cause = "pending", ""
		case "failed", "held", "partial":
			c.Cause = waitCause(d)
			c.Fragment = c.Fragment || fragment(d)
			if c.Fragment && c.Attempts >= firstPOSTs {
				// One save: an intent record with four POSTs would read as due.
				markUnresolved(l, id, c, reasonExhausted, p.opts.Now())
				exhausted = true
				return
			}
			c.Status, c.Reason = "intent", reasonRetry
			if c.Attempts >= firstPOSTs {
				c.NextAttempt = p.opts.Now().UTC().Add(p.retryWait(c.Attempts))
			}
		}
	})
	if err == nil && exhausted {
		p.opts.Logger.Warn("inbound delivery unresolved", "change", id, "cause", reasonExhausted)
		err = p.opts.NeverSilent.Stop(id, false)
	}
	if err == nil && status == "no_route" {
		err = p.opts.NeverSilent.Stop(id, true)
	}
	return err
}

// fragment reports an answer that says part of the message is in a terminal:
// "partial", or "held" with a "partial" member.
func fragment(d wire.Delivery) bool {
	if d.Status == "partial" {
		return true
	}
	if d.Status != "held" {
		return false
	}
	for _, m := range d.Members {
		if m.Status == "partial" {
			return true
		}
	}
	return false
}

// unresolved ends a change for an operator and records its cause.
func (p *Processor) unresolved(id, reason string) error {
	if reason == reasonWithdrawn {
		if err := p.opts.NeverSilent.Forget(id); err != nil {
			return err
		}
	} else {
		l, err := p.snapshot()
		if err != nil {
			return err
		}
		c := l.Changes[id]
		definite := strings.HasPrefix(reason, "gc rejected the POST:")
		if c != nil && c.Reason == "retry" && !c.Fragment && (c.Cause == causeNotDelivered || c.Cause == causeDraftDeferred || c.Cause == causePendingInteraction || c.Cause == causeBehindEarlier) {
			definite = true
		}
		if err := p.opts.NeverSilent.Stop(id, definite); err != nil {
			return err
		}
	}
	recorded := false
	err := p.update(func(l *ledger.Ledger) {
		c := l.Changes[id]
		if c == nil {
			return
		}
		markUnresolved(l, id, c, reason, p.opts.Now())
		recorded = true
	})
	if err == nil && recorded {
		p.opts.Logger.Warn("inbound delivery unresolved", "change", id, "cause", reason)
	}
	return err
}

// markUnresolved runs inside an update; Changes and Unresolved hold the record.
func markUnresolved(l *ledger.Ledger, id string, c *ledger.ChangeState, reason string, at time.Time) {
	c.NoticeAt = at
	c.Status = "unresolved"
	c.Reason = reason
	c.Cause = unresolvedCause(reason, c.Cause)
	c.NextAttempt = time.Time{}
	l.Unresolved[id] = c
}

func deliverySessions(d wire.Delivery) []string {
	var sessions []string
	for _, m := range d.Members {
		if m.SessionID != "" {
			sessions = append(sessions, m.SessionID)
		}
	}
	return sessions
}

func wait(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func mentions(m *api.Message, id string) bool {
	for _, a := range m.Annotations {
		if a != nil && a.Type == "USER_MENTION" && a.UserMention != nil && a.UserMention.User != nil && a.UserMention.User.Name == id {
			return true
		}
	}
	return false
}

func renderMentions(m *api.Message) string {
	// Annotation describes spans in Message.Text (for example @FooBot), not
	// global substitutions of user-resource tokens. Interpret offsets as UTF-16
	// code units and map only complete rune boundaries back to Go byte offsets.
	// The REST reference does not explicitly state its Unicode indexing unit:
	// https://developers.google.com/workspace/chat/api/reference/rest/v1/spaces.messages#Annotation
	boundaries := make(map[int64]int)
	var units int64
	for offset, r := range m.Text {
		boundaries[units] = offset
		units++
		if r > 0xffff {
			units++
		}
	}
	boundaries[units] = len(m.Text)
	type span struct {
		start, end int
		name       string
	}
	var spans []span
	for _, a := range m.Annotations {
		if a == nil || a.Type != "USER_MENTION" || a.UserMention == nil || a.UserMention.User == nil {
			continue
		}
		if a.StartIndex < 0 || a.StartIndex > units || a.Length <= 0 || a.Length > units-a.StartIndex {
			continue
		}
		start, startOK := boundaries[a.StartIndex]
		end, endOK := boundaries[a.StartIndex+a.Length]
		if !startOK || !endOK {
			continue // never split a surrogate pair
		}
		// Without a display name, keep the literal span (usually @Name).
		name := a.UserMention.User.DisplayName
		if name != "" {
			spans = append(spans, span{start, end, name})
		}
	}
	sort.SliceStable(spans, func(i, j int) bool { return spans[i].start < spans[j].start })
	var text strings.Builder
	pos := 0
	for _, s := range spans {
		if s.start < pos {
			continue // first valid span wins; duplicates/overlaps cannot corrupt text
		}
		text.WriteString(m.Text[pos:s.start])
		text.WriteString(s.name)
		pos = s.end
	}
	text.WriteString(m.Text[pos:])
	return text.String()
}

// NormalizeContent is shared by inbound turns and on-demand history. Uploaded
// files use the same bounded, symlink-safe download path; failures remain visible
// in the text instead of dropping the attachment or the message.
func NormalizeContent(ctx context.Context, client *chat.Client, endpoints config.Endpoints, m *api.Message) (string, []wire.Attachment) {
	w := wire.InboundMessage{Text: renderMentions(m)}
	appendAttachments(ctx, client, endpoints, m, &w)
	return w.Text, w.Attachments
}

// All normalizer callers share accounting through download completion. A single
// process-wide gate also covers config reloads and aliases of the media root.
var mediaNormalizationGate = make(chan struct{}, 1)

func appendAttachments(ctx context.Context, client *chat.Client, endpoints config.Endpoints, m *api.Message, w *wire.InboundMessage) {
	if len(m.Attachment) == 0 {
		return
	}
	used := map[string]bool{}
	// The store has one quota for every file kept, and a message is capped by
	// count as well as by bytes: a sender chooses both. A file that would
	// cross the quota is refused, never at the cost of one already kept — the
	// agent may still be reading it.
	var stored int64
	var usageErr error
	select {
	case mediaNormalizationGate <- struct{}{}:
		defer func() { <-mediaNormalizationGate }()
		stored, usageErr = media.Usage(endpoints.MediaDir)
	case <-ctx.Done():
		usageErr = ctx.Err()
	}
	downloads := 0
	for i, a := range m.Attachment {
		if a == nil {
			continue
		}
		name := a.ContentName
		if name == "" {
			name = fmt.Sprintf("attachment-%d", i+1)
		}
		item := wire.Attachment{ProviderID: a.Name, MIMEType: attachmentMIMEType(a)}
		reason := ""
		switch {
		case a.DriveDataRef != nil && a.DriveDataRef.DriveFileId != "":
			item.URL = "https://drive.google.com/file/d/" + url.PathEscape(a.DriveDataRef.DriveFileId) + "/view"
		case a.AttachmentDataRef != nil && a.AttachmentDataRef.ResourceName != "":
			if usageErr != nil {
				reason = "media accounting unavailable"
				break
			}
			if endpoints.MediaMaxAttachments > 0 && downloads >= endpoints.MediaMaxAttachments {
				reason = "attachment limit reached"
				break
			}
			if filepath.Base(name) != name || name == "." || name == ".." || strings.ContainsAny(name, "\\\x00") {
				reason = "invalid filename"
				break
			}
			filename := name
			if used[filename] {
				filename = fmt.Sprintf("%d-%s", i+1, name)
			}
			used[filename] = true
			parts := strings.Split(m.Name, "/")
			if len(parts) != 4 || parts[0] != "spaces" || parts[2] != "messages" {
				reason = "invalid message name"
				break
			}
			dir, err := mediaDirectory(endpoints.MediaDir, parts[1], parts[3])
			if err != nil {
				reason = "media directory unavailable"
				break
			}
			dest := filepath.Join(dir, filename)
			// Never trust an unrecorded existing file (it may be an interrupted
			// download). Keep it and choose a fresh exclusive filename instead.
			if _, err := os.Lstat(dest); !errors.Is(err, os.ErrNotExist) {
				dest = filepath.Join(dir, rand.Text()+"-"+filename)
			}
			limit := endpoints.MediaMaxBytes
			quotaBound := false
			if endpoints.MediaMaxTotalBytes > 0 {
				remaining := endpoints.MediaMaxTotalBytes - stored
				if remaining <= 0 {
					reason = "storage quota reached"
					break
				}
				if limit <= 0 || remaining < limit {
					limit = remaining
					quotaBound = true
				}
			}
			written, err := client.DownloadMedia(ctx, a.AttachmentDataRef.ResourceName, dest, limit)
			if err != nil {
				reason = mediaReason(err)
				if quotaBound && strings.Contains(err.Error(), "byte limit") {
					reason = "storage quota reached"
				}
			} else {
				item.URL = (&url.URL{Scheme: "file", Path: dest}).String()
				stored += written
				downloads++
			}
		default:
			reason = "attachment data unavailable"
		}
		line := "[attachment] " + name
		if reason != "" {
			line += " unavailable: " + reason
		} else {
			if a.ContentType != "" {
				line += " (" + a.ContentType + ")"
			}
			line += " " + item.URL
		}
		if w.Text != "" {
			w.Text += "\n"
		}
		w.Text += line
		w.Attachments = append(w.Attachments, item)
	}
}

// attachmentMIMEType never returns an empty value: gc's inbound schema requires
// mime_type on every attachment, and Chat omits contentType for Drive links
// (driveDataRef) and occasionally for uploads. A Drive link whose real type is
// unknown without a Drive API call carries Google's generic Drive file type.
func attachmentMIMEType(a *api.Attachment) string {
	if a.ContentType != "" {
		return a.ContentType
	}
	if a.DriveDataRef != nil && a.DriveDataRef.DriveFileId != "" {
		return "application/vnd.google-apps.file"
	}
	return "application/octet-stream"
}

func mediaReason(err error) string {
	if strings.Contains(err.Error(), "byte limit") {
		return "size limit exceeded"
	}
	kind, _ := chat.Classify(err)
	switch kind {
	case "auth":
		return "access denied"
	case "not_found":
		return "file not found"
	}
	return "download or storage failed"
}

// Walk from the filesystem root using directory handles, refusing symlinks in
// the configured root and every child. Use a canonical path on systems with
// symlinked temporary directories, as required by the puller's media storage.
func mediaDirectory(root string, parts ...string) (string, error) {
	if root == "" {
		return "", errors.New("media root missing")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	abs = filepath.Join(append([]string{abs}, parts...)...)
	volume := filepath.VolumeName(abs) + string(filepath.Separator)
	dir, err := os.OpenRoot(volume)
	if err != nil {
		return "", err
	}
	defer func() { dir.Close() }()
	for _, part := range strings.Split(strings.TrimPrefix(abs, volume), string(filepath.Separator)) {
		if part == "" {
			continue
		}
		if err := dir.Mkdir(part, 0700); err != nil && !errors.Is(err, os.ErrExist) {
			return "", err
		}
		before, err := dir.Lstat(part)
		if err != nil || !before.IsDir() {
			return "", errors.New("media path is not a directory")
		}
		next, err := dir.OpenRoot(part)
		if err != nil {
			return "", err
		}
		after, err := next.Stat(".")
		if err != nil || !os.SameFile(before, after) {
			next.Close()
			return "", errors.New("media directory changed")
		}
		dir.Close()
		dir = next
	}
	return abs, nil
}

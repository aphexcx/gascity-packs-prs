// Package catchup recovers frozen history intervals through the inbound ledger.
package catchup

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/pubsub/v2"
	"gchat-adapter/internal/chat"
	"gchat-adapter/internal/config"
	"gchat-adapter/internal/inbound"
	"gchat-adapter/internal/ledger"
	"gchat-adapter/internal/puller"
	api "google.golang.org/api/chat/v1"
	"google.golang.org/api/googleapi"
)

type Options struct {
	Config *config.Config
	Ledger *ledger.Ledger
	// LedgerMu is shared with inbound and every other ledger consumer.
	LedgerMu   *sync.Mutex
	LedgerPath string
	Chat       *chat.Client
	Processor  *inbound.Processor
	Logger     puller.Logger
	// Save runs under LedgerMu, saving the snapshot without modifying it.
	Save func(*ledger.Ledger) error
	Now  func() time.Time
}

type Runner struct {
	opts   Options
	budget int
	gate   chan struct{}
}

// progress is JSON in SpaceState.PageToken: the API's opaque token needs its
// stream, frozen query bounds and enumeration phase to be resumable. Backlog
// holds JSON queuedChanges, including payloads not yet handed to inbound.
// No other ledger consumer should interpret these two opaque fields.
type progress struct {
	Version      int       `json:"version"`
	Phase        string    `json:"phase"`
	Token        string    `json:"token,omitempty"`
	MessageLower time.Time `json:"message_lower"`
	EventLower   time.Time `json:"event_lower"`
	Reported     bool      `json:"reported,omitempty"`
	NextUpper    time.Time `json:"next_upper,omitzero"`
	// Freeze retries access after a listing rejoin or a one-hour hold. Keep
	// the summary until a successful page so repeated denials are not new holds.
	Unavailable      string    `json:"unavailable,omitempty"`
	Credentials      string    `json:"credentials,omitempty"`
	ListingAbsent    bool      `json:"listing_absent,omitempty"`
	RetryUnavailable bool      `json:"retry_unavailable,omitempty"`
	AccessRetryAfter time.Time `json:"access_retry_after,omitzero"`
}

func (p progress) unavailableHeld() bool {
	return p.Unavailable != "" && !p.RetryUnavailable
}

type queuedChange struct {
	Change  puller.Change `json:"change"`
	At      time.Time     `json:"at"`
	Handled bool          `json:"handled,omitempty"`
	// Malformed history is retained as an unresolved record, just like a
	// quarantined live change, rather than blocking the space forever.
	Malformed string `json:"malformed,omitempty"`
}

var eventTypes = []string{
	"google.workspace.chat.message.v1.updated",
	"google.workspace.chat.message.v1.deleted",
	"google.workspace.chat.reaction.v1.created",
	"google.workspace.chat.reaction.v1.deleted",
}

var errSpaceUnavailable = errors.New("catchup: space unavailable")
var errUnchanged = errors.New("catchup: ledger unchanged")

func New(opts Options) (*Runner, error) {
	if opts.Config == nil || opts.Ledger == nil || opts.LedgerMu == nil || opts.Chat == nil || opts.Processor == nil || (opts.Save == nil && opts.LedgerPath == "") {
		return nil, errors.New("catchup: config, clients, processor, ledger, shared mutex and persistence are required")
	}
	if opts.Config.CatchupBudget() <= 0 {
		return nil, errors.New("catchup: budget must be positive")
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Save == nil {
		opts.Save = func(l *ledger.Ledger) error { return l.Save(opts.LedgerPath) }
	}
	return &Runner{opts: opts, budget: opts.Config.CatchupBudget(), gate: make(chan struct{}, 1)}, nil
}

// Freeze refreshes all spaces and starts intervals before the live stream runs.
// An unfinished interval survives a restart unchanged. A newer Freeze queues
// its upper bound so Pass also recovers the gap through the latest restart.
func (r *Runner) Freeze(ctx context.Context, now time.Time) error {
	if err := r.enter(ctx); err != nil {
		return err
	}
	defer r.leave()
	if now.IsZero() {
		return errors.New("catchup: a frozen upper bound is required")
	}
	spaces, err := r.opts.Chat.ListSpaces(ctx)
	if err != nil {
		return fmt.Errorf("catchup: refresh spaces: %w", err)
	}
	var reported, clockAhead []string
	err = r.update(func(l *ledger.Ledger) error {
		listed := make(map[string]bool, len(spaces))
		for _, space := range spaces {
			listed[space.Name] = true
			s := l.Spaces[space.Name]
			if s == nil {
				s = &ledger.SpaceState{Wake: string(r.opts.Config.Wake(space.Name))}
				l.Spaces[space.Name] = s
			}
			if s.Wake == "" {
				s.Wake = string(r.opts.Config.Wake(space.Name))
			}
			if space.SpaceType == "DIRECT_MESSAGE" || space.Type == "DM" {
				s.Kind = "dm"
			} else {
				s.Kind = "room"
			}
		}
		for name, s := range l.Spaces {
			if s == nil {
				continue
			}
			if !s.FrozenUpper.IsZero() {
				p, err := decodeProgress(s.PageToken)
				if err != nil {
					return err
				}
				if p.Unavailable != "" && listed[name] && (p.ListingAbsent || !now.Before(p.AccessRetryAfter)) {
					p.RetryUnavailable = true
				}
				p.ListingAbsent = !listed[name]
				if now.After(s.FrozenUpper) && now.After(p.NextUpper) {
					p.NextUpper = now.UTC()
				}
				s.PageToken = encode(p)
				continue
			}
			if now.Before(s.MessageCursor) || now.Before(s.EventCursor) {
				clockAhead = append(clockAhead, name)
				continue
			}
			p := r.begin(s, now)
			p.ListingAbsent = !listed[name]
			s.PageToken = encode(p)
			if p.Reported {
				l.Counters["unrecoverable:"+name]++
				reported = append(reported, name)
			}
		}
		return nil
	})
	if err == nil {
		for _, name := range clockAhead {
			r.opts.Logger.Warn("catch-up skipped space with cursor ahead of clock", "space", name)
		}
		for _, name := range reported {
			r.report(name)
		}
	}
	return err
}

func (r *Runner) begin(s *ledger.SpaceState, upper time.Time) progress {
	p := progress{Version: 1, Phase: "messages", MessageLower: overlap(s.MessageCursor), EventLower: overlap(s.EventCursor)}
	// Request the full retained event window. Message history has no 28-day
	// limit. Report only a known cursor older than the retention boundary.
	earliest := r.opts.Now().UTC().Add(-28 * 24 * time.Hour)
	if p.EventLower.Before(earliest) {
		p.EventLower = earliest
		p.Reported = !s.EventCursor.IsZero() && s.EventCursor.Before(earliest)
	}
	s.FrozenUpper, s.PageToken = upper.UTC(), encode(p)
	return p
}

// Pass enumerates each interval into a durable backlog and delivers at most the
// configured budget per space. remaining includes pending receipts. On a page
// error it counts only retained records; the error signals unenumerated history.
// Poll belongs to inbound: an Ack with a pending receipt cannot move a cursor.
func (r *Runner) Pass(ctx context.Context) (delivered, remaining int, err error) {
	if err = r.enter(ctx); err != nil {
		return
	}
	defer r.leave()
	l, err := r.snapshot()
	if err != nil {
		return 0, 0, err
	}
	names := make([]string, 0, len(l.Spaces))
	for name := range l.Spaces {
		names = append(names, name)
	}
	sort.Strings(names)
	var failures []error
	for _, name := range names {
		if l.Spaces[name] == nil || l.Spaces[name].FrozenUpper.IsZero() {
			continue
		}
		if err := ctx.Err(); err != nil {
			failures = append(failures, err)
			break
		}
		budget := r.budget
		for {
			enumerationErr := r.enumerate(ctx, name)
			if enumerationErr != nil {
				failures = append(failures, enumerationErr)
				// An unread event page may precede every retained message. Hold
				// delivery until both streams are complete to preserve oldest-first.
				break
			}
			n, err := r.drain(ctx, name, &budget)
			delivered += n
			if err != nil {
				failures = append(failures, err)
				break
			}
			current, err := r.snapshot()
			if err != nil {
				failures = append(failures, err)
				break
			}
			s, err := spaceState(current, name)
			if err != nil {
				failures = append(failures, err)
				break
			}
			if s.FrozenUpper.IsZero() {
				break
			}
			p, err := decodeProgress(s.PageToken)
			if err != nil {
				failures = append(failures, err)
				break
			}
			if len(s.Backlog) > 0 || p.unavailableHeld() {
				break
			}
			// A queued restart interval starts here, sharing this pass's budget.
			// Enumerate even at zero budget so remaining exposes its backlog.
		}
	}
	l, err = r.snapshot()
	if err != nil {
		failures = append(failures, err)
	} else {
		for _, s := range l.Spaces {
			if s != nil {
				remaining += len(s.Backlog)
			}
		}
	}
	return delivered, remaining, errors.Join(failures...)
}

func (r *Runner) enumerate(ctx context.Context, name string) error {
	// Refresh retention once on entering the events phase, then keep the query
	// fixed across its pages. A resumed enumeration checks the boundary again.
	var earliest time.Time
	var restartedPhase string
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		l, err := r.snapshot()
		if err != nil {
			return err
		}
		s, err := spaceState(l, name)
		if err != nil {
			return err
		}
		p, err := decodeProgress(s.PageToken)
		if err != nil {
			return err
		}
		if p.Phase == "done" || p.unavailableHeld() {
			return nil
		}
		var page []queuedChange
		var next string
		if p.Phase == "messages" {
			var messages []*api.Message
			messages, next, err = r.opts.Chat.ListMessagesPage(ctx, name, p.MessageLower, s.FrozenUpper, "", p.Token)
			for _, message := range messages {
				at := time.Time{}
				if message != nil {
					at, _ = time.Parse(time.RFC3339Nano, message.CreateTime)
				}
				page = append(page, queuedChange{Change: puller.Change{Kind: "message.created", Space: name, Message: message}, At: at})
			}
		} else {
			// A long pause can age out the saved event query. Restart only that
			// stream, retain its backlog, and report the lost history once.
			if earliest.IsZero() {
				earliest = r.opts.Now().UTC().Add(-28 * 24 * time.Hour)
			}
			if p.EventLower.Before(earliest) {
				p.EventLower, p.Token = earliest, ""
				lost := !s.EventCursor.IsZero() && s.EventCursor.Before(earliest)
				if err := r.saveProgress(name, p, lost); err != nil {
					return err
				}
				p.Reported = p.Reported || lost
			}
			if p.EventLower.Before(s.FrozenUpper) {
				var events []*api.SpaceEvent
				lower := p.EventLower
				if !lower.After(earliest.Add(5 * time.Minute)) {
					// Let Google measure its own retained window. Keep our bound
					// in progress for resume and loss reporting, not on the wire.
					// A small margin also tolerates clock skew near retention.
					lower = time.Time{}
				}
				events, next, err = r.opts.Chat.ListSpaceEventsPage(ctx, name, lower, s.FrozenUpper, eventTypes, p.Token)
				for _, event := range events {
					page = append(page, expandEvent(name, event)...)
				}
			}
		}
		if err != nil {
			kind, _ := chat.Classify(err)
			var apiErr *googleapi.Error
			if kind == "auth" || (kind == "not_found" && errors.As(err, &apiErr) && apiErr.Code == 403) {
				return r.holdAccess(name, p, err)
			}
			if kind == "permanent" && p.Token != "" {
				// A token can expire while access is held. Keep the frozen query
				// and retained records; inbound and unavailable IDs absorb replay.
				p.Token = ""
				if saveErr := r.saveProgress(name, p, false); saveErr != nil {
					return saveErr
				}
				if restartedPhase == p.Phase {
					// Bound retries if even replacement tokens fail. A later pass
					// retries from the first page without reporting history loss.
					return fmt.Errorf("catchup: list %s for %s: %w", p.Phase, name, err)
				}
				restartedPhase = p.Phase
				continue
			}
			if kind == "not_found" || kind == "permanent" {
				if saveErr := r.saveProgress(name, p, true); saveErr != nil {
					return saveErr
				}
			}
			return fmt.Errorf("catchup: list %s for %s: %w", p.Phase, name, err)
		}
		if next != "" && next == p.Token {
			return errors.New("catchup: history page token did not advance")
		}
		p.Token = next
		p.Credentials = ""
		p.Unavailable, p.RetryUnavailable, p.AccessRetryAfter = "", false, time.Time{}
		if next == "" {
			if p.Phase == "messages" {
				p.Phase = "events"
			} else {
				p.Phase = "done"
			}
		}
		if err := r.update(func(l *ledger.Ledger) error {
			s, err := spaceState(l, name)
			if err != nil {
				return err
			}
			// Restarted phases replay earlier pages. Keep one durable record
			// per history identity, even when a replacement token keeps failing.
			seen := make(map[string]bool, len(s.Backlog)+len(page))
			for _, raw := range s.Backlog {
				var item queuedChange
				if err := json.Unmarshal([]byte(raw), &item); err != nil {
					return errors.New("catchup: invalid durable backlog")
				}
				seen[unavailableID(item)] = true
			}
			for _, item := range page {
				id := unavailableID(item)
				if seen[id] {
					continue
				}
				s.Backlog = append(s.Backlog, encode(item))
				seen[id] = true
			}
			s.PageToken = encode(p)
			return nil
		}); err != nil {
			return err
		}
	}
}

func (r *Runner) drain(ctx context.Context, name string, budget *int) (delivered int, err error) {
	// A fully enumerated interval can contain both messages and older space
	// events. Order the retained records before delivering any of them.
	if err := r.update(func(l *ledger.Ledger) error {
		s, err := spaceState(l, name)
		if err != nil {
			return err
		}
		p, err := decodeProgress(s.PageToken)
		if err != nil {
			return err
		}
		if p.unavailableHeld() || p.Phase != "done" {
			return errUnchanged
		}
		items := make([]queuedChange, len(s.Backlog))
		for i, raw := range s.Backlog {
			if err := json.Unmarshal([]byte(raw), &items[i]); err != nil {
				return errors.New("catchup: invalid durable backlog")
			}
		}
		less := func(i, j int) bool { return items[i].At.Before(items[j].At) }
		if sort.SliceIsSorted(items, less) {
			return errUnchanged
		}
		sort.SliceStable(items, less)
		for i, item := range items {
			s.Backlog[i] = encode(item)
		}
		return nil
	}); err != nil {
		return 0, err
	}
	index := 0
	for {
		if err := ctx.Err(); err != nil {
			return delivered, err
		}
		before, err := r.snapshot()
		if err != nil {
			return delivered, err
		}
		s, err := spaceState(before, name)
		if err != nil {
			return delivered, err
		}
		p, err := decodeProgress(s.PageToken)
		if err != nil {
			return delivered, err
		}
		if p.unavailableHeld() || p.Phase != "done" {
			return delivered, nil
		}
		if index >= len(s.Backlog) {
			break
		}
		var item queuedChange
		if err := json.Unmarshal([]byte(s.Backlog[index]), &item); err != nil {
			return delivered, errors.New("catchup: invalid durable backlog")
		}
		// An overlap after rejoining must never post history already retained
		// locally when access was lost, even if its resource payload changed.
		if retained := before.Changes[unavailableID(item)]; retained != nil && retained.Status == "committed" {
			item.Handled = true
		}
		if !item.Handled {
			if *budget <= 0 {
				break
			}
			outcome := puller.Quarantine
			if item.Malformed == "" {
				outcome, err = r.opts.Processor.HandleChange(ctx, item.Change)
			}
			if outcome == puller.Quarantine {
				if err = r.quarantine(name, item); err != nil {
					return delivered, err
				}
			} else if err != nil {
				return delivered, err
			} else if outcome != puller.Ack {
				return delivered, errors.New("catchup: inbound change was not acknowledged")
			}
			item.Handled = true
		}
		after, err := r.snapshot()
		if err != nil {
			return delivered, err
		}
		for id, c := range after.Changes {
			if !related(item.Change, c) {
				continue
			}
			old := before.Changes[id]
			if old != nil && old.Status != "intent" {
				continue
			}
			switch c.Reason {
			case "own", "local", "wake_policy", "space unavailable", "parent unavailable":
				continue
			}
			*budget--
			if c.Status == "committed" && c.Reason == "delivered" {
				delivered++
			}
		}
		terminal := true
		for _, c := range after.Changes {
			if related(item.Change, c) && c.Status != "committed" && c.Status != "unresolved" {
				terminal = false
				break
			}
		}
		if err := r.update(func(l *ledger.Ledger) error {
			s, err := spaceState(l, name)
			if err != nil {
				return err
			}
			if terminal {
				s.Backlog = append(s.Backlog[:index], s.Backlog[index+1:]...)
			} else {
				s.Backlog[index] = encode(item)
			}
			return nil
		}); err != nil {
			return delivered, err
		}
		if !terminal {
			index++
		}
	}
	reported := false
	err = r.update(func(l *ledger.Ledger) error {
		s, err := spaceState(l, name)
		if err != nil {
			return err
		}
		if len(s.Backlog) == 0 {
			p, err := decodeProgress(s.PageToken)
			if err != nil {
				return err
			}
			// Neither cursor may advance until both streams' last pages are saved.
			if p.Phase != "done" {
				return errUnchanged
			}
			s.MessageCursor, s.EventCursor = s.FrozenUpper, s.FrozenUpper
			s.PageToken, s.FrozenUpper = "", time.Time{}
			if p.NextUpper.After(s.MessageCursor) {
				next := r.begin(s, p.NextUpper)
				next.ListingAbsent = p.ListingAbsent
				s.PageToken = encode(next)
				reported = next.Reported
				if reported {
					l.Counters["unrecoverable:"+name]++
				}
			}
			return nil
		}
		return errUnchanged
	})
	if err == nil && reported {
		r.report(name)
	}
	return delivered, err
}

// Inbound may map an update-before-create to the base ID, or suppress a stale
// revision entirely. Check the ledger's actual outcomes instead of inventing a
// second copy of its ID rules. A pending change for this message conservatively
// holds its recovered records; unrelated live messages never hold the cursor.
func related(c puller.Change, state *ledger.ChangeState) bool {
	if c.Message != nil {
		return state.Message == c.Message.Name && strings.HasPrefix(state.Kind, "message.")
	}
	if c.Reaction != nil {
		return state.ID == c.Reaction.Name+"#"+strings.TrimPrefix(c.Kind, "reaction.")
	}
	return false
}

func expandEvent(space string, event *api.SpaceEvent) []queuedChange {
	raw := encode(event)
	bad := func() []queuedChange { return []queuedChange{{Malformed: raw}} }
	if event == nil {
		return bad()
	}
	at, err := time.Parse(time.RFC3339Nano, event.EventTime)
	if err != nil {
		return bad()
	}
	parts := strings.Split(strings.TrimPrefix(event.EventType, "google.workspace.chat."), ".v1.")
	if len(parts) != 2 || parts[1] == "" {
		return bad()
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &fields); err != nil {
		return bad()
	}
	key := parts[0] + strings.ToUpper(parts[1][:1]) + parts[1][1:] + "EventData"
	ev, err := puller.Decode(&pubsub.Message{Attributes: map[string]string{"ce-type": event.EventType}, Data: fields[key]})
	if err != nil || len(ev.Changes) == 0 {
		return bad()
	}
	items := make([]queuedChange, 0, len(ev.Changes))
	for _, c := range ev.Changes {
		if c.Space != space {
			return bad()
		}
		items = append(items, queuedChange{Change: c, At: at})
	}
	return items
}

func (r *Runner) quarantine(space string, item queuedChange) error {
	raw := encode(item)
	id := fmt.Sprintf("catchup:%x", sha256.Sum256([]byte(space+raw)))
	return r.update(func(l *ledger.Ledger) error {
		if l.Changes[id] != nil {
			return nil
		}
		c := &ledger.ChangeState{ID: id, Space: space, Kind: "catchup.malformed", Status: "unresolved", Created: r.opts.Now().UTC(), Event: raw, Reason: "malformed catch-up history"}
		l.Changes[id], l.Unresolved[id] = c, c
		return nil
	})
}

func (r *Runner) saveProgress(name string, p progress, lost bool) error {
	report := lost && !p.Reported
	if report {
		p.Reported = true
	}
	err := r.update(func(l *ledger.Ledger) error {
		s, err := spaceState(l, name)
		if err != nil {
			return err
		}
		s.PageToken = encode(p)
		if report {
			l.Counters["unrecoverable:"+name]++
		}
		return nil
	})
	if err == nil && report {
		r.report(name)
	}
	return err
}

func (r *Runner) holdAccess(name string, p progress, cause error) error {
	kind, _ := chat.Classify(cause)
	summary := kind
	var apiErr *googleapi.Error
	if errors.As(cause, &apiErr) {
		summary = fmt.Sprintf("%s (HTTP %d)", kind, apiErr.Code)
	}
	key, sentinel, first := "unavailable:", errSpaceUnavailable, p.Unavailable == ""
	retrying := p.RetryUnavailable
	if kind == "auth" {
		key, sentinel, first = "credentials:", inbound.ErrCredentials, p.Credentials == ""
		p.Credentials = summary
	} else {
		p.Unavailable = summary
		p.RetryUnavailable = false
		p.AccessRetryAfter = r.opts.Now().UTC().Add(time.Hour)
	}
	held := fmt.Errorf("catchup: list %s for %s: %s: %w", p.Phase, name, summary, sentinel)
	err := r.update(func(l *ledger.Ledger) error {
		if !first && (kind == "auth" || !retrying) {
			return errUnchanged
		}
		s, err := spaceState(l, name)
		if err != nil {
			return err
		}
		if kind != "auth" {
			for _, raw := range s.Backlog {
				var item queuedChange
				if err := json.Unmarshal([]byte(raw), &item); err != nil {
					return errors.New("catchup: invalid durable backlog")
				}
				id := unavailableID(item)
				if l.Changes[id] == nil {
					l.Changes[id] = &ledger.ChangeState{ID: id, Space: name, Kind: "catchup.unavailable", Status: "committed", Created: r.opts.Now().UTC(), Event: raw, Reason: "space unavailable: " + summary}
				}
			}
			s.Backlog = nil
		}
		s.PageToken = encode(p)
		if first {
			l.Counters[key+name]++
		}
		return nil
	})
	if err != nil {
		return errors.Join(held, err)
	}
	if first {
		r.opts.Logger.Warn("catch-up access unavailable", "space", name, "cause", summary)
	}
	return held
}

// This is a local history-retention key, not an inbound provider-message ID.
// Bind it to the resource and its stream time, excluding mutable message text.
func unavailableID(item queuedChange) string {
	resource := item.Malformed
	if item.Change.Message != nil {
		resource = item.Change.Message.Name
	} else if item.Change.Reaction != nil {
		resource = item.Change.Reaction.Name
	}
	key := encode([]string{item.Change.Kind, resource, item.At.UTC().Format(time.RFC3339Nano)})
	return fmt.Sprintf("catchup:unavailable:%x", sha256.Sum256([]byte(key)))
}

func (r *Runner) report(name string) {
	r.opts.Logger.Warn("catch-up history unrecoverable", "space", name)
}

func overlap(cursor time.Time) time.Time {
	if cursor.IsZero() {
		return time.Unix(0, 0).UTC()
	}
	return cursor.Add(-time.Minute)
}

func decodeProgress(raw string) (progress, error) {
	var p progress
	if err := json.Unmarshal([]byte(raw), &p); err != nil || p.Version != 1 || p.MessageLower.IsZero() || p.EventLower.IsZero() || (p.Phase != "messages" && p.Phase != "events" && p.Phase != "done") {
		return p, errors.New("catchup: invalid durable pagination progress")
	}
	return p, nil
}

func encode(v any) string {
	// Only local structs and Google resource structs are encoded. None contain
	// unsupported JSON values; the return is always a complete durable record.
	b, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("catchup: encode record: %v", err))
	}
	return string(b)
}

func (r *Runner) enter(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case r.gate <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (r *Runner) leave() { <-r.gate }

func clone(l *ledger.Ledger) (*ledger.Ledger, error) {
	return l.Clone(), nil
}

func (r *Runner) snapshot() (*ledger.Ledger, error) {
	r.opts.LedgerMu.Lock()
	defer r.opts.LedgerMu.Unlock()
	return clone(r.opts.Ledger)
}

func spaceState(l *ledger.Ledger, name string) (*ledger.SpaceState, error) {
	s := l.Spaces[name]
	if s == nil {
		return nil, fmt.Errorf("catchup: space state missing for %s", name)
	}
	return s, nil
}

// Copy-on-save prevents a failed disk write from publishing in-memory progress.
func (r *Runner) update(fn func(*ledger.Ledger) error) error {
	r.opts.LedgerMu.Lock()
	defer r.opts.LedgerMu.Unlock()
	l, err := clone(r.opts.Ledger)
	if err != nil {
		return err
	}
	if err := fn(l); err != nil {
		if errors.Is(err, errUnchanged) {
			return nil
		}
		return err
	}
	if err := r.opts.Save(l); err != nil {
		return errors.New("catchup: ledger save failed")
	}
	*r.opts.Ledger = *l
	return nil
}

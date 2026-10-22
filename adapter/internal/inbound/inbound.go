// Package inbound maps Chat changes to durable gc turns. Ambiguous deliveries
// are retained for operator resolution; they are never automatically replayed.
package inbound

import (
	"context"
	"crypto/rand"
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
	"gchat-adapter/internal/puller"
	"gchat-adapter/internal/wire"
	api "google.golang.org/api/chat/v1"
)

type Options struct {
	Config *config.Config
	Ledger *ledger.Ledger
	// LedgerMu must be the same mutex used by every other ledger consumer.
	LedgerMu       *sync.Mutex
	LedgerPath     string
	GC             *gcclient.Client
	Chat           *chat.Client
	IdentityUserID string
	Logger         puller.Logger
	// Save optionally replaces Ledger.Save. It runs with LedgerMu held and must
	// durably save the supplied snapshot without retaining or modifying it.
	Save func(*ledger.Ledger) error
	// RetryDelay defaults to one second; retries back off exponentially.
	RetryDelay time.Duration
}

type Processor struct {
	opts Options
	gate chan struct{}
}

func New(opts Options) (*Processor, error) {
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
	if opts.Save == nil {
		opts.Save = func(l *ledger.Ledger) error { return l.Save(opts.LedgerPath) }
	}
	return &Processor{opts: opts, gate: make(chan struct{}, 1)}, nil
}

// Handle is a puller.Handler. Pending receipts are durable recovery obligations,
// so they permit Ack; Poll must run regularly (normally every five seconds).
// Network requests never hold LedgerMu. Handle and Poll serialize their work,
// including live and catch-up callers, and waiting callers honor cancellation.
func (p *Processor) Handle(ctx context.Context, ev *puller.Event) (puller.Outcome, error) {
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
		if err := p.change(ctx, c); err != nil {
			return puller.Nack, err
		}
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
// after startup. Receipt lookup failures retain pending state for a later pass.
func (p *Processor) Poll(ctx context.Context) error {
	if err := p.enter(ctx); err != nil {
		return err
	}
	defer p.leave()
	l, err := p.snapshot()
	if err != nil {
		return err
	}
	ids := make([]string, 0)
	for id, c := range l.Changes {
		if c.Status == "pending" || c.Status == "intent" {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool {
		a, b := l.Changes[ids[i]], l.Changes[ids[j]]
		if a.Created.Equal(b.Created) {
			return a.ID < b.ID
		}
		return a.Created.Before(b.Created)
	})
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
			continue
		}
		switch r.State {
		case "pending":
			continue
		case "concluded":
			if r.Delivery.Status == "pending" {
				err = p.unresolved(id, "receipt concluded with pending delivery")
			} else {
				err = p.outcome(id, r.Delivery.Status, "")
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
	b, err := json.Marshal(l)
	if err != nil {
		return nil, errors.New("inbound: ledger snapshot failed")
	}
	var result ledger.Ledger
	if err := json.Unmarshal(b, &result); err != nil {
		return nil, errors.New("inbound: ledger snapshot failed")
	}
	return &result, nil
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

func (p *Processor) change(ctx context.Context, c puller.Change) error {
	switch {
	case strings.HasPrefix(c.Kind, "membership."):
		m := c.Membership
		if c.Kind == "membership.deleted" || m.Member == nil || m.Member.Name != p.identity() || (m.State != "" && m.State != "JOINED") {
			return nil
		}
		return p.update(func(l *ledger.Ledger) {
			if l.Spaces[c.Space] == nil {
				l.Spaces[c.Space] = &ledger.SpaceState{Wake: string(p.opts.Config.Wake(c.Space))}
			}
		})
	case strings.HasPrefix(c.Kind, "reaction."):
		return p.reaction(ctx, c)
	default:
		return p.message(ctx, c)
	}
}

var errSpaceUnavailable = errors.New("inbound: space type unavailable")

func permanentLookup(err error) bool {
	kind, _ := chat.Classify(err)
	return errors.Is(err, errSpaceUnavailable) || kind == "not_found" || kind == "auth" || kind == "permanent"
}

func (p *Processor) space(ctx context.Context, name string, supplied *api.Space) (*ledger.SpaceState, error) {
	l, err := p.snapshot()
	if err != nil {
		return nil, err
	}
	s := l.Spaces[name]
	if s == nil {
		s = &ledger.SpaceState{Wake: string(p.opts.Config.Wake(name))}
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

func (p *Processor) message(ctx context.Context, c puller.Change) error {
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
	}
	// Deleting a never-observed message still needs a tombstone, but no turn.
	if c.Kind == "message.deleted" && previous == nil {
		return p.record(c, id, wire.InboundMessage{}, &state, nil, "local")
	}
	s, err := p.space(ctx, c.Space, m.Space)
	if err != nil {
		if permanentLookup(err) {
			w := p.envelope(id, c.Space, "", m.Sender, state.Thread)
			w.Text = renderMentions(m)
			return p.record(c, id, w, &state, nil, "space unavailable")
		}
		return errors.New("inbound: space lookup failed")
	}
	w := p.envelope(id, c.Space, s.Kind, m.Sender, state.Thread)
	w.Text = renderMentions(m)
	forwarded := false
	for _, change := range l.Changes {
		if change.Message == m.Name && strings.HasPrefix(change.Kind, "message.") && change.Reason != "local" && change.Reason != "own" && change.Reason != "wake_policy" && change.Reason != "space unavailable" {
			forwarded = true
			break
		}
	}
	local := ""
	if state.Mine {
		local = "own"
	} else if c.Kind == "message.deleted" {
		w.Text = "deleted"
		if base != nil {
			var original wire.InboundMessage
			if json.Unmarshal([]byte(base.Event), &original) == nil {
				w.Actor = original.Actor
			}
		}
		if !forwarded {
			local = "local"
		}
	} else if !forwarded && s.Kind != "dm" && s.Wake == "mentions" && !mentions(m, p.identity()) {
		local = "wake_policy"
	}
	if c.Kind == "message.updated" && id != m.Name {
		w.Text = "edited: " + w.Text
	}
	if local == "" && c.Kind != "message.deleted" {
		p.attachments(ctx, m, &w)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := p.record(c, id, w, &state, s, local); err != nil {
		return err
	}
	return p.resume(ctx, id)
}

func (p *Processor) reaction(ctx context.Context, c puller.Change) error {
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
	state := l.Messages[parent]
	if state == nil {
		m, err := p.opts.Chat.GetMessage(ctx, parent)
		if err != nil {
			if permanentLookup(err) {
				w := p.envelope(id, c.Space, "", r.User, "")
				w.Text = reactionText(r, action, parent)
				return p.record(c, id, w, nil, nil, "parent unavailable")
			}
			return errors.New("inbound: reaction parent lookup failed")
		}
		state = &ledger.MessageState{Mine: m.Sender != nil && m.Sender.Name == p.identity()}
		if m.Thread != nil {
			state.Thread = m.Thread.Name
		}
	}
	s, err := p.space(ctx, c.Space, nil)
	if err != nil {
		if permanentLookup(err) {
			w := p.envelope(id, c.Space, "", r.User, state.Thread)
			w.Text = reactionText(r, action, parent)
			return p.record(c, id, w, state, nil, "space unavailable")
		}
		return errors.New("inbound: space lookup failed")
	}
	w := p.envelope(id, c.Space, s.Kind, r.User, state.Thread)
	w.Text = reactionText(r, action, parent)
	if action == "deleted" && r.User == nil {
		if created := l.Changes[r.Name+"#created"]; created != nil {
			var original wire.InboundMessage
			if json.Unmarshal([]byte(created.Event), &original) == nil {
				w.Actor = original.Actor
				w.Text = strings.Replace(original.Text, "reaction created:", "reaction deleted:", 1)
			}
		}
	}
	local := ""
	if !state.Mine {
		local = "local"
	}
	if err := p.record(c, id, w, state, s, local); err != nil {
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
		Actor: a, ReplyToMessageID: thread, ReceivedAt: time.Now().UTC()}
}

func (p *Processor) record(c puller.Change, id string, w wire.InboundMessage, state *ledger.MessageState, space *ledger.SpaceState, local string) error {
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
	return p.update(func(l *ledger.Ledger) {
		status := "intent"
		if local != "" {
			status = "committed"
		}
		l.Changes[id] = &ledger.ChangeState{ID: id, Space: c.Space, Message: name, Kind: c.Kind, Status: status, Created: time.Now().UTC(), Event: string(data), Reason: local}
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
			} else {
				l.Spaces[c.Space] = space
			}
		}
	})
}

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
			return p.unresolved(id, "message deleted or superseded before retry")
		}
		if c.Attempts > 0 && c.Reason != "retry" {
			return p.unresolved(id, "interrupted POST; delivery unknown")
		}
		if c.Attempts >= 4 {
			return p.unresolved(id, "delivery retries exhausted")
		}
		if c.Attempts > 0 {
			if err := wait(ctx, p.opts.RetryDelay*time.Duration(1<<uint(c.Attempts-1))); err != nil {
				return err
			}
		}
		var w wire.InboundMessage
		if json.Unmarshal([]byte(c.Event), &w) != nil || w.ProviderMessageID != id {
			return p.unresolved(id, "stored turn is malformed")
		}
		// Persist the attempt before crossing the process boundary. A crash or
		// lost response after this point is ambiguous, never safe to repost.
		if err := p.update(func(l *ledger.Ledger) { l.Changes[id].Attempts++; l.Changes[id].Reason = "" }); err != nil {
			return err
		}
		r, err := p.opts.GC.Inbound(ctx, w)
		if err != nil {
			var responseErr *gcclient.HTTPError
			answered := errors.As(err, &responseErr)
			var op *net.OpError
			if (answered && (responseErr.StatusCode >= 500 || responseErr.StatusCode == 429)) || (errors.As(err, &op) && op.Op == "dial") {
				if err := p.update(func(l *ledger.Ledger) { l.Changes[id].Attempts--; l.Changes[id].Reason = "retry" }); err != nil {
					return err
				}
				return errors.New("inbound: gc temporarily unavailable; retry POST")
			}
			if answered && responseErr.StatusCode >= 400 && responseErr.StatusCode < 500 {
				// The status and machine-readable code describe a definite rejection.
				// Never log HTTPError itself: it includes the response body.
				reason := fmt.Sprintf("gc rejected the POST: HTTP %d", responseErr.StatusCode)
				if code := responseErr.Code(); code != "" {
					reason += " " + code
				}
				return p.unresolved(id, reason)
			}
			if err := p.unresolved(id, "POST response unavailable; delivery unknown"); err != nil {
				return err
			}
			return ctx.Err()
		}
		if err := p.outcome(id, r.Delivery.Status, r.Delivery.ReceiptID); err != nil {
			return err
		}
	}
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

func (p *Processor) outcome(id, status, receipt string) error {
	if status == "pending" && receipt == "" {
		return p.unresolved(id, "pending delivery has no receipt")
	}
	switch status {
	case "delivered", "no_route", "pending", "failed", "partial":
		return p.update(func(l *ledger.Ledger) {
			c := l.Changes[id]
			if receipt != "" {
				c.ReceiptID = receipt
			}
			switch status {
			case "delivered", "no_route":
				if c.Status != "committed" {
					l.Counters["turns:"+c.Space]++
					if status == "no_route" {
						l.Counters["no_route:"+c.Space]++
					}
				}
				c.Status = "committed"
				c.Reason = status
			case "pending":
				c.Status = "pending"
			case "failed", "partial":
				c.Status = "intent"
				c.Reason = "retry"
			}
		})
	default:
		return p.unresolved(id, "unrecognized delivery status")
	}
}

func (p *Processor) unresolved(id, reason string) error {
	err := p.update(func(l *ledger.Ledger) {
		c := l.Changes[id]
		c.Status = "unresolved"
		c.Reason = reason
		l.Unresolved[id] = c
	})
	if err == nil {
		p.opts.Logger.Warn("inbound delivery unresolved", "change", id, "cause", reason)
	}
	return err
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
	text := m.Text
	for _, a := range m.Annotations {
		if a == nil || a.Type != "USER_MENTION" || a.UserMention == nil || a.UserMention.User == nil {
			continue
		}
		u := a.UserMention.User
		name := u.DisplayName
		if name == "" {
			name = u.Name
		}
		text = strings.ReplaceAll(text, "<"+u.Name+">", name)
	}
	return text
}

func (p *Processor) attachments(ctx context.Context, m *api.Message, w *wire.InboundMessage) {
	used := map[string]bool{}
	for i, a := range m.Attachment {
		if a == nil {
			continue
		}
		name := a.ContentName
		if name == "" {
			name = fmt.Sprintf("attachment-%d", i+1)
		}
		item := wire.Attachment{ProviderID: a.Name, MIMEType: a.ContentType}
		reason := ""
		switch {
		case a.DriveDataRef != nil && a.DriveDataRef.DriveFileId != "":
			item.URL = "https://drive.google.com/file/d/" + url.PathEscape(a.DriveDataRef.DriveFileId) + "/view"
		case a.AttachmentDataRef != nil && a.AttachmentDataRef.ResourceName != "":
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
			dir, err := mediaDirectory(p.opts.Config.Endpoints.MediaDir, parts[1], parts[3])
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
			_, err = p.opts.Chat.DownloadMedia(ctx, a.AttachmentDataRef.ResourceName, dest, p.opts.Config.Endpoints.MediaMaxBytes)
			if err != nil {
				reason = mediaReason(err)
			} else {
				item.URL = (&url.URL{Scheme: "file", Path: dest}).String()
			}
		default:
			reason = "attachment data unavailable"
		}
		line := "[attachment] " + name
		if reason != "" {
			line += " unavailable: " + reason
		} else {
			line += " (" + a.ContentType + ") " + item.URL
		}
		if w.Text != "" {
			w.Text += "\n"
		}
		w.Text += line
		w.Attachments = append(w.Attachments, item)
	}
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

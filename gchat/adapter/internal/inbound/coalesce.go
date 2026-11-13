package inbound

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"gchat-adapter/internal/ledger"
	"gchat-adapter/internal/provenance"
	"gchat-adapter/internal/puller"
	"gchat-adapter/internal/wire"
)

const windowActLimit = 50
const windowByteLimit = 32000

// HandleChanges records a catch-up pass under the same gate, then freezes and
// delivers its turns without a live window. Validation remains all-or-nothing.
func (p *Processor) HandleChanges(ctx context.Context, changes []puller.Change) (puller.Outcome, error) {
	return p.handle(ctx, &puller.Event{Changes: changes}, true)
}

func actTime(c puller.Change, now time.Time) time.Time {
	if at := c.ActTime(); !at.IsZero() {
		return at.UTC()
	}
	return now.UTC()
}

func isHeld(c *ledger.ChangeState) bool {
	return c != nil && c.Status == "intent" && c.Attempts == 0 && !c.HeldUntil.IsZero()
}

func cancelHeld(c *ledger.ChangeState) {
	c.Status, c.Reason, c.HeldUntil = "committed", "coalesced_cancelled", time.Time{}
}

// hold is part of record's atomic save, before the transport may acknowledge.
// Folding never changes a turn whose first POST could have left the process.
func (p *Processor) hold(l *ledger.Ledger, input puller.Change, id string, w wire.InboundMessage) {
	c := l.Changes[id]
	if p.opts.Config.CoalesceWindow() == 0 || c.Status != "intent" {
		return
	}
	c.ActAt = actTime(input, p.opts.Now())
	if w.Conversation.Kind == "dm" && strings.HasPrefix(c.Kind, "message.") && !c.CatchUp {
		return
	}
	c.HeldUntil = c.Created.Add(p.opts.Config.CoalesceWindow())
	for _, other := range l.Changes {
		if other.ID != id && isHeld(other) && other.Space == c.Space && other.CatchUp == c.CatchUp && other.HeldUntil.Before(c.HeldUntil) {
			c.HeldUntil = other.HeldUntil
		}
	}
	if c.CatchUp {
		c.HeldUntil = c.Created
	}
	if input.Reaction != nil && c.Kind == "reaction.deleted" {
		if old := l.Changes[input.Reaction.Name+"#created"]; isHeld(old) && old.CatchUp == c.CatchUp {
			cancelHeld(old)
			cancelHeld(c)
		}
		return
	}
	if c.Kind != "message.updated" && c.Kind != "message.deleted" {
		return
	}
	var previous *ledger.ChangeState
	for _, other := range l.Changes {
		if other.ID != id && isHeld(other) && other.Message == c.Message && other.CatchUp == c.CatchUp && strings.HasPrefix(other.OriginalKind(), "message.") {
			if previous == nil || earlier(previous, other) {
				previous = other
			}
		}
	}
	if previous == nil {
		return
	}
	if c.Kind == "message.deleted" {
		cancelHeld(previous)
		if previous.OriginalKind() == "message.created" {
			cancelHeld(c)
		}
		return
	}
	if previous.OriginalKind() == "message.created" {
		// Preserve the create ID and timestamp, with the last body. The special
		// kind keeps a rollback's superseded check from withdrawing this fold.
		w.ProviderMessageID = previous.ID
		if previous.MemberKind == "" {
			previous.MemberKind = previous.Kind
		}
		previous.Kind = "coalesced"
		data, _ := json.Marshal(w)
		previous.Event = string(data)
		c.Status, c.Reason, c.MemberOf, c.HeldUntil = "committed", "coalesced", previous.ID, time.Time{}
	} else {
		c.HeldUntil = previous.HeldUntil
		previous.Status, previous.Reason, previous.MemberOf, previous.HeldUntil = "committed", "coalesced", c.ID, time.Time{}
	}
}

func (p *Processor) admit(ctx context.Context, input puller.Change, id string, catchUp bool) error {
	if p.opts.Config.CoalesceWindow() == 0 {
		return p.resume(ctx, id)
	}
	l, err := p.snapshot()
	if err != nil {
		return err
	}
	c := l.Changes[id]
	for c != nil && c.MemberOf != "" {
		c = l.Changes[c.MemberOf]
	}
	if c == nil || c.Status != "intent" {
		return nil
	}
	urgent := false
	if input.Message != nil {
		urgent = mentions(input.Message, p.identity())
		if input.Message.Thread != nil {
			for name, m := range l.Messages {
				if strings.HasPrefix(name, input.Space+"/messages/") && m.Mine && m.Thread != "" && m.Thread == input.Message.Thread.Name {
					urgent = true
					break
				}
			}
		}
	}
	// Catch-up is collected under handle's gate and flushed at the pass end.
	if catchUp {
		return p.flushSpace(ctx, c.Space, true, false, "")
	}
	anchor := ""
	if urgent {
		anchor = c.ID
	}
	if err := p.flushSpace(ctx, c.Space, false, urgent || !isHeld(c), anchor); err != nil {
		return err
	}
	if !isHeld(c) {
		return p.resume(ctx, c.ID)
	}
	return nil
}

func (p *Processor) held(l *ledger.Ledger, space string, catchUp bool) []*ledger.ChangeState {
	var acts []*ledger.ChangeState
	for _, c := range l.Changes {
		if isHeld(c) && c.Space == space && c.CatchUp == catchUp {
			acts = append(acts, c)
		}
	}
	sort.Slice(acts, func(i, j int) bool { return earlier(acts[i], acts[j]) })
	return acts
}

func (p *Processor) flushSpace(ctx context.Context, space string, catchUp, force bool, anchor string) error {
	for {
		l, err := p.snapshot()
		if err != nil {
			return err
		}
		acts := p.held(l, space, catchUp)
		if len(acts) == 0 {
			return nil
		}
		n := min(len(acts), windowActLimit)
		for i := 2; i <= n; i++ {
			w, _, err := p.merged(acts[:i], anchor)
			if err != nil {
				return err
			}
			if len(w.Text) > windowByteLimit {
				n = i - 1
				break
			}
		}
		first, _, err := p.merged(acts[:1], anchor)
		if err != nil {
			return err
		}
		capReached := n < len(acts) || n == windowActLimit || len(first.Text) > windowByteLimit
		due := !p.opts.Now().Before(acts[0].HeldUntil)
		// While recording catch-up, the enclosing pass decides when it ends.
		if catchUp {
			due = false
		}
		for _, c := range acts[:n] {
			due = due || p.recoverHeld[c.ID]
		}
		if !force && !due && !capReached {
			return nil
		}
		w, owner, err := p.merged(acts[:n], anchor)
		if err != nil {
			return err
		}
		data, err := json.Marshal(w)
		if err != nil {
			return err
		}
		if err = p.update(func(l *ledger.Ledger) {
			c := l.Changes[owner]
			if n > 1 {
				c.MemberEvent, c.MemberKind = c.Event, c.OriginalKind()
				c.Kind = "coalesced"
				c.Members = make([]string, 0, n)
				c.Created = acts[0].Created
				for _, a := range acts[:n] {
					m := l.Changes[a.ID]
					c.Members = append(c.Members, m.ID)
					m.HeldUntil = time.Time{}
					if m.ID != owner {
						m.Status, m.Reason, m.MemberOf = "committed", "coalesced", owner
					}
				}
			}
			c.Event = string(data)
			c.HeldUntil = time.Time{}
		}); err != nil {
			return err
		}
		for _, c := range acts[:n] {
			delete(p.recoverHeld, c.ID)
		}
		if err = p.resume(ctx, owner); err != nil {
			return err
		}
		// An overflow's last act starts the next turn without borrowing the old
		// window's end. Its Created still bounds it if more acts arrive later.
		if !force && n < len(acts) {
			next := acts[n].Created.Add(p.opts.Config.CoalesceWindow())
			if err = p.update(func(l *ledger.Ledger) {
				for _, a := range acts[n:] {
					if c := l.Changes[a.ID]; isHeld(c) {
						c.HeldUntil = next
					}
				}
			}); err != nil {
				return err
			}
		}
	}
}

func (p *Processor) merged(input []*ledger.ChangeState, anchor string) (wire.InboundMessage, string, error) {
	acts := append([]*ledger.ChangeState(nil), input...)
	sort.Slice(acts, func(i, j int) bool {
		if acts[i].ActAt.Equal(acts[j].ActAt) {
			return acts[i].ID < acts[j].ID
		}
		return acts[i].ActAt.Before(acts[j].ActAt)
	})
	owner := acts[len(acts)-1]
	for _, c := range acts {
		if c.OriginalKind() == "message.created" {
			owner = c
		}
	}
	for _, c := range acts {
		if c.ID == anchor {
			owner = c
			break
		}
	}
	var w wire.InboundMessage
	if json.Unmarshal([]byte(owner.Event), &w) != nil {
		return w, "", errors.New("inbound: malformed held turn")
	}
	if len(acts) == 1 {
		return w, owner.ID, nil
	}
	var text strings.Builder
	oldest, newest := acts[0].ActAt, acts[len(acts)-1].ActAt
	if owner.CatchUp || p.opts.Now().Sub(oldest) > time.Minute {
		prefix := ""
		if owner.CatchUp {
			prefix = "catch-up: "
		}
		fmt.Fprintf(&text, "[%s%d messages, oldest first; %s to %s UTC. Each has its own from line.]", prefix, len(acts), oldest.UTC().Format("2006-01-02 15:04"), newest.UTC().Format("2006-01-02 15:04"))
	} else {
		fmt.Fprintf(&text, "[%d messages in this room, oldest first. Each has its own from line.]", len(acts))
	}
	w.Attachments = nil
	for _, c := range acts {
		var member wire.InboundMessage
		if json.Unmarshal([]byte(c.Event), &member) != nil {
			return w, "", errors.New("inbound: malformed held member")
		}
		text.WriteByte('\n')
		text.WriteString(provenance.QuoteMergedHeaders(strings.TrimPrefix(member.Text, "[catch-up]\n")))
		w.Attachments = append(w.Attachments, member.Attachments...)
	}
	w.Text = text.String()
	return w, owner.ID, nil
}

// flushWindows is called under the inbound gate, before live or catch-up work.
func (p *Processor) flushWindows(ctx context.Context) error {
	if p.opts.Config.CoalesceWindow() == 0 && len(p.recoverHeld) == 0 {
		return nil
	}
	l, err := p.snapshot()
	if err != nil {
		return err
	}
	names := map[string]bool{}
	for _, c := range l.Changes {
		if isHeld(c) {
			names[c.Space] = true
		}
	}
	ordered := make([]string, 0, len(names))
	for name := range names {
		ordered = append(ordered, name)
	}
	sort.Strings(ordered)
	for _, name := range ordered {
		if err = p.flushSpace(ctx, name, false, false, ""); err != nil {
			return err
		}
		if err = p.flushSpace(ctx, name, true, true, ""); err != nil {
			return err
		}
	}
	return nil
}

func (p *Processor) signalWindow() {
	if p.opts.Config.CoalesceWindow() == 0 {
		return
	}
	select {
	case p.windowChanged <- struct{}{}:
	default:
	}
}

// RunWindows owns only scheduling; all obligations and deadlines live in the
// ledger. Cancellation stops without flushing. No request context owns a timer.
func (p *Processor) RunWindows(ctx context.Context) error {
	for {
		if err := p.enter(ctx); err != nil {
			return err
		}
		err := p.flushWindows(ctx)
		delay := 24 * time.Hour
		l, snapshotErr := p.snapshot()
		if snapshotErr == nil {
			for _, c := range l.Changes {
				if isHeld(c) {
					delay = min(delay, max(c.HeldUntil.Sub(p.opts.Now()), 0))
				}
			}
		}
		p.leave()
		if err != nil || snapshotErr != nil {
			p.opts.Logger.Warn("inbound window deferred; retained for retry")
			delay = time.Second
		}
		if err = p.windowWait(ctx, delay, p.windowChanged); err != nil {
			return err
		}
	}
}

func windowWait(ctx context.Context, d time.Duration, changed <-chan struct{}) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-changed:
		return nil
	case <-t.C:
		return nil
	}
}

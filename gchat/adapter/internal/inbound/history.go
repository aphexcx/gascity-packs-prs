package inbound

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"gchat-adapter/internal/config"
	"gchat-adapter/internal/ledger"
	"gchat-adapter/internal/principal"
	"gchat-adapter/internal/provenance"
	"gchat-adapter/internal/puller"
	"gchat-adapter/internal/wire"
	api "google.golang.org/api/chat/v1"
)

// HistorySkipCause is empty for unbounded spaces and current acts.
func HistorySkipCause(l *ledger.Ledger, c puller.Change) string {
	bound := ledger.HistoryBound(l.Spaces[c.Space], l.HistoryFloor)
	if bound.IsZero() {
		return ""
	}
	at := c.ActTime()
	if at.IsZero() {
		return "history_time_missing"
	}
	if at.Before(bound) {
		return "before_history_floor"
	}
	return ""
}

func historyID(c puller.Change) string {
	name := ""
	if c.Message != nil {
		name = c.Message.Name
		if c.Kind == "message.created" && !c.ActTime().IsZero() {
			return name
		}
	}
	if c.Reaction != nil {
		name = c.Reaction.Name
	}
	return name + "#history#" + c.Kind + "#" + c.ActTime().UTC().Format(time.RFC3339Nano)
}

// Use the event-time fallback in bounded message revision IDs as well as in
// the bound. Preserve malformed resource times so validation quarantines them.
func historyTime(c puller.Change) puller.Change {
	if c.Message == nil || c.ActTime().IsZero() {
		return c
	}
	raw := c.Message.CreateTime
	if c.Kind == "message.updated" {
		raw = c.Message.LastUpdateTime
	}
	if raw != "" {
		return c
	}
	m := *c.Message
	if c.Kind == "message.created" {
		m.CreateTime = c.ActTime().UTC().Format(time.RFC3339Nano)
	}
	if c.Kind == "message.updated" {
		m.LastUpdateTime = c.ActTime().UTC().Format(time.RFC3339Nano)
	}
	c.Message = &m
	return c
}

func historyUser(c puller.Change) *api.User {
	if c.Message != nil {
		return c.Message.Sender
	}
	if c.Reaction != nil {
		return c.Reaction.User
	}
	return nil
}

// RetireQueuedHistory removes the delivery state of one old queued act before
// load-time migration retains its frame-only skip. A handled queue row can
// still have an intent, receipt or unresolved record that Poll would resume.
// Never erase an already committed delivery or another revision of the item.
func RetireQueuedHistory(l *ledger.Ledger, cfg *config.Config, c puller.Change) {
	if HistorySkipCause(l, c) == "" {
		return
	}
	id := ""
	if c.Message != nil {
		id = c.Message.Name
		switch c.Kind {
		case "message.updated":
			baseID := id
			id += "#updated#" + c.Message.LastUpdateTime
			if l.Changes[id] == nil {
				if base := l.Changes[baseID]; base != nil && base.OriginalKind() == c.Kind && base.Created.Before(ledger.HistoryBound(l.Spaces[c.Space], l.HistoryFloor)) {
					id = baseID
				}
			}
		case "message.deleted":
			id += "#deleted"
		}
	} else if c.Reaction != nil {
		id = c.Reaction.Name + "#" + strings.TrimPrefix(c.Kind, "reaction.")
	}
	if old := l.Changes[id]; old != nil && old.OriginalKind() == c.Kind && old.Status != "committed" {
		if !preserveHistoryNotice(l, cfg, old) {
			// The queued row retires only this act. Restore unsent members before
			// removing their owner so the space migration judges each one too.
			var members []*ledger.ChangeState
			for _, member := range l.Changes {
				if member.ID != id && member.MemberOf != "" && l.Delivery(member) == old {
					members = append(members, member)
				}
			}
			for _, member := range members {
				member.Event, member.Kind = member.OriginalEvent(), member.OriginalKind()
				member.MemberEvent, member.MemberKind, member.MemberOf, member.Members = "", "", "", nil
				member.Status, member.Reason, member.HeldUntil = "intent", "", time.Time{}
			}
			delete(l.Changes, id)
			delete(l.Unresolved, id)
		}
		if c.Kind == "message.deleted" {
			if state := l.Messages[c.Message.Name]; state != nil {
				state.Deleted = false
			}
		}
	}
}

// Keep the original notice key across a restart. A retired turn has no wire
// message ID: its frame can be inspected but cannot be replayed as a POST.
func preserveHistoryNotice(l *ledger.Ledger, cfg *config.Config, c *ledger.ChangeState) bool {
	if !c.RaisedEnd.IsZero() {
		return false
	}
	switch c.Status {
	case "unresolved":
		if c.Cause != causeUnknown || (c.Reason == reasonRetryBound && waitingMailed(c)) {
			return false
		}
	case "intent":
		if c.Attempts == 0 || c.Reason == reasonRetry {
			return false
		}
		c.Reason = "interrupted POST; delivery unknown"
	case "pending":
		if c.ReceiptID == "" {
			return false
		}
		c.Reason = "receipt unavailable or unknown"
	default:
		return false
	}
	c.Event = redactedHistoryEvent(l, cfg, c, "")
	c.MemberEvent = ""
	c.ProvenanceFramed = true
	c.Status, c.Cause = "unresolved", causeUnknown
	c.NextAttempt = time.Time{}
	l.Unresolved[c.ID] = c
	return true
}

func redactedHistoryEvent(l *ledger.Ledger, cfg *config.Config, old *ledger.ChangeState, wireID string) string {
	var w wire.InboundMessage
	_ = json.Unmarshal([]byte(old.OriginalEvent()), &w)
	frame, framed := provenance.FrameOf(w.Text)
	if !old.ProvenanceFramed || !framed {
		user := &api.User{Name: w.Actor.ID, DisplayName: w.Actor.DisplayName}
		frame = provenance.Frame(HistorySpeaker(l, cfg, puller.Change{Message: &api.Message{Sender: user}}))
	}
	w.Text, w.Attachments, w.ProviderMessageID = frame, nil, wireID
	if old.CatchUp {
		w.Text = "[catch-up]\n" + w.Text
	}
	raw, _ := json.Marshal(w)
	return string(raw)
}

// RetireSpaceHistory handles attempts that outlived their catch-up queue row.
// An attempt recorded before a newly installed bound necessarily predates it;
// its Created time is an upper bound on the act, not a replacement event time.
// Existing cursors never call this migration, and committed deliveries survive.
// A merged turn is judged by its Created, its first act's time. A turn with an
// act on the other side of the bound is first split back into its acts, so
// each act is judged by its own time. So is an edit folded into its message's
// creation.
func RetireSpaceHistory(l *ledger.Ledger, cfg *config.Config, space string, log puller.Logger) []string {
	bound := ledger.HistoryBound(l.Spaces[space], l.HistoryFloor)
	if bound.IsZero() {
		return nil
	}
	for _, owner := range l.Changes {
		if owner.Space == space && dividedAt(l, owner, bound) {
			unmerge(l, owner)
		}
	}
	unfoldLaterEdits(l, space, bound)
	var retired []string
	// Select before mutating owners: committed members of an undelivered
	// coalesced turn are still undelivered acts, independent of map order.
	var retiring []*ledger.ChangeState
	for _, old := range l.Changes {
		if old.Space != space || l.Delivery(old).Status == "committed" || !beforeBound(old.Created, bound) ||
			(!strings.HasPrefix(old.OriginalKind(), "message.") && !strings.HasPrefix(old.OriginalKind(), "reaction.")) {
			continue
		}
		retiring = append(retiring, old)
	}
	for _, old := range retiring {
		id, kind := old.ID, old.OriginalKind()
		if kind == "message.deleted" {
			if state := l.Messages[old.Message]; state != nil {
				state.Deleted = false
			}
		}
		if preserveHistoryNotice(l, cfg, old) {
			continue
		}
		retained := *old
		retained.Kind = kind
		retained.MemberEvent, retained.MemberKind, retained.MemberOf, retained.Members, retained.MemberCreated = "", "", "", nil, time.Time{}
		retained.MemberRevision, retained.HeldUntil = time.Time{}, time.Time{}
		if kind != "message.created" {
			retained.ID += "#history#" + kind + "#" + old.Created.UTC().Format(time.RFC3339Nano)
		}
		retained.Event = redactedHistoryEvent(l, cfg, old, retained.ID)
		retained.Status, retained.Reason, retained.Cause = "committed", "local", "before_history_floor"
		retained.ReceiptID, retained.NextAttempt = "", time.Time{}
		retained.ProvenanceFramed = true
		if old.Created.IsZero() {
			retained.Cause = "history_time_missing"
			l.Counters["history_time_missing:"+space]++
			if log != nil {
				log.Warn("inbound act has no usable history time", "space", space, "kind", kind)
			}
		}
		delete(l.Changes, id)
		delete(l.Unresolved, id)
		l.Changes[retained.ID] = &retained
		l.Counters["history_delivery_retired:"+space]++
		retired = append(retired, old.Message)
	}
	return retired
}

// beforeBound is the one comparison of a recorded time with a history bound.
// A record with no recorded time counts as before it.
func beforeBound(at, bound time.Time) bool {
	return at.IsZero() || at.Before(bound)
}

// rewritable is the delivery state in which refreshMerged may rewrite a turn:
// never POSTed, or every POST answered "not delivered". The POST of any other
// state may have landed, so posting its acts again could post them twice.
func rewritable(c *ledger.ChangeState) bool {
	return c.Status == "intent" && (c.Attempts == 0 || c.Reason == reasonRetry)
}

// dividedAt reports a frozen, rewritable turn whose Created is before the
// bound and one of whose acts was recorded at or after it. The Created is its
// first act's, or after a refresh the time of an act that has left it. A turn
// whose Created is at or after the bound stays whole, as a single act does: an
// operator's replay records it anew.
func dividedAt(l *ledger.Ledger, owner *ledger.ChangeState, bound time.Time) bool {
	if len(owner.Members) == 0 || !rewritable(owner) || !beforeBound(owner.Created, bound) {
		return false
	}
	later := false
	for _, id := range owner.Members {
		m := l.Changes[id]
		if m == nil || (m != owner && m.MemberOf != owner.ID) {
			return false
		}
		later = later || !beforeBound(m.OriginalCreated(), bound)
	}
	return later
}

// unmerge undoes the window flush's merge: each act is again a single intent
// with its own text, kind and recorded time. The owner keeps the attempts gc
// refused, and the old turn's text is gone in the same save.
func unmerge(l *ledger.Ledger, owner *ledger.ChangeState) {
	for _, id := range owner.Members {
		if m := l.Changes[id]; m != owner {
			m.Status, m.Reason, m.MemberOf = "intent", "", ""
		}
	}
	owner.Event, owner.Kind, owner.Created = owner.OriginalEvent(), owner.OriginalKind(), owner.OriginalCreated()
	owner.MemberEvent, owner.MemberKind, owner.MemberCreated, owner.Members = "", "", time.Time{}, nil
}

// unfoldLaterEdits gives an edit back its own record when the window folded it
// into its message's held creation (hold), the creation was recorded before
// the bound and the edit at or after it. The creation's record carries the
// latest edit's text, so only the latest such edit leaves it; the earlier ones
// were superseded by it. A creation whose delivery may have landed keeps them.
func unfoldLaterEdits(l *ledger.Ledger, space string, bound time.Time) {
	latest := map[string]*ledger.ChangeState{}
	for _, edit := range l.Changes {
		c := l.Changes[edit.MemberOf]
		if edit.Space != space || edit.Status != "committed" || edit.Reason != "coalesced" || edit.OriginalKind() != "message.updated" ||
			c == nil || c.Message != edit.Message || c.OriginalKind() != "message.created" ||
			!beforeBound(c.OriginalCreated(), bound) || beforeBound(edit.Created, bound) || !rewritable(l.Delivery(c)) {
			continue
		}
		if last := latest[c.ID]; last == nil || earlier(last, edit) {
			latest[c.ID] = edit
		}
	}
	for _, edit := range latest {
		edit.Status, edit.Reason, edit.MemberOf = "intent", "", ""
	}
}

// RetainHistorySkip is also used while migrating a saved queue, before network
// clients start. It retains only the ordinary envelope and attribution frame.
// Reason local is understood by released readers; Cause is diagnostic only.
// It returns true only when a new record was added, so retries do not recount.
func RetainHistorySkip(l *ledger.Ledger, cfg *config.Config, c puller.Change, now time.Time, speaker provenance.Speaker, catchUp bool, identity string) bool {
	cause := HistorySkipCause(l, c)
	if cause == "" {
		return false
	}
	id := historyID(c)
	if l.Changes[id] != nil {
		return false
	}
	name, thread := "", ""
	var state *ledger.MessageState
	if c.Message != nil {
		m := c.Message
		name = m.Name
		state = &ledger.MessageState{}
		if previous := l.Messages[name]; previous != nil {
			*state = *previous
		}
		if m.Thread != nil {
			state.Thread = m.Thread.Name
		}
		if m.Sender != nil {
			state.Mine = m.Sender.Name == identity
		}
		thread = state.Thread
		speaker.Item = provenance.ChatItem(name)
	} else if c.Reaction != nil {
		name = strings.Split(c.Reaction.Name, "/reactions/")[0]
		if previous := l.Messages[name]; previous != nil {
			thread = previous.Thread
		}
	}
	user := historyUser(c)
	actor := wire.Actor{}
	if user != nil {
		actor = wire.Actor{ID: user.Name, DisplayName: user.DisplayName, IsBot: user.Type == "BOT"}
		if actor.DisplayName == "" {
			actor.DisplayName = actor.ID
		}
	}
	kind := ""
	if s := l.Spaces[c.Space]; s != nil {
		kind = s.Kind
	}
	w := wire.InboundMessage{ProviderMessageID: id, Conversation: wire.Conversation{ScopeID: cfg.Secrets.CityName, Provider: "gchat", AccountID: cfg.Identity().Name, ConversationID: c.Space, Kind: kind}, Actor: actor, ReplyToMessageID: thread, ReceivedAt: now.UTC(), Text: provenance.Frame(speaker)}
	if user == nil {
		if base := l.Changes[name]; base != nil {
			var original wire.InboundMessage
			if json.Unmarshal([]byte(base.Event), &original) == nil {
				w.Actor = original.Actor
				if frame, ok := provenance.FrameOf(original.Text); base.ProvenanceFramed && ok {
					w.Text = frame
				}
			}
		}
	}
	if catchUp {
		w.Text = "[catch-up]\n" + w.Text
	}
	raw, _ := json.Marshal(w)
	l.Changes[id] = &ledger.ChangeState{ID: id, Space: c.Space, Message: name, Kind: c.Kind, Status: "committed", Created: now.UTC(), Event: string(raw), Reason: "local", Cause: cause, CatchUp: catchUp, ProvenanceFramed: true}
	if state != nil {
		l.Messages[name] = state
	}
	if cause == "history_time_missing" {
		l.Counters["history_time_missing:"+c.Space]++
	}
	return true
}

// HistorySpeaker needs no network at load. Live callers use their cached
// directory resolver before entering the shared ledger update.
func HistorySpeaker(l *ledger.Ledger, cfg *config.Config, c puller.Change) provenance.Speaker {
	r := principal.New(principal.Options{Owners: cfg.Secrets.Owners, OrganizationDomain: cfg.Secrets.OrganizationDomain})
	return r.Speaker(context.Background(), historyUser(c), l.IdentityDomainID)
}

// firstReadAdmits lifts only the join part of the bound for an adding message.
func firstReadAdmits(l *ledger.Ledger, c puller.Change, identity, kind string) bool {
	s := l.Spaces[c.Space]
	if s == nil || s.FirstRead == nil || s.FirstRead.Complete || !s.HistoryStart.IsZero() || c.Kind != "message.created" || c.Message == nil {
		return false
	}
	at := c.ActTime()
	return at.After(s.FirstRead.Lower) && at.After(l.HistoryFloor) && at.Before(s.JoinedAt) &&
		(kind == "dm" || mentions(c.Message, identity))
}

func (p *Processor) history(ctx context.Context, c puller.Change, catchUp bool) (bool, error) {
	l, err := p.snapshot()
	if err != nil {
		return false, err
	}
	s := l.Spaces[c.Space]
	if s == nil {
		s = &ledger.SpaceState{Wake: string(p.opts.Config.Wake(c.Space))}
		if c.Message != nil && c.Message.Space != nil {
			s.Kind = spaceKind(c.Message.Space)
		}
	}
	if l.StartSpace(s, p.opts.Now()) || l.Spaces[c.Space] == nil {
		if err := p.update(func(current *ledger.Ledger) {
			if current.Spaces[c.Space] == nil {
				current.Spaces[c.Space] = s
			} else {
				current.StartSpace(current.Spaces[c.Space], p.opts.Now())
			}
		}); err != nil {
			return false, err
		}
		l, err = p.snapshot()
		if err != nil {
			return false, err
		}
	}
	cause := HistorySkipCause(l, c)
	if cause == "before_history_floor" && firstReadAdmits(l, c, p.identity(), "dm") {
		// Only the space's kind can still refuse it: look it up as message()
		// does. A failed lookup records nothing; message() repeats it, as on main.
		space, err := p.resolveSpace(ctx, c.Space, c.Message.Space, true)
		if err != nil || firstReadAdmits(l, c, p.identity(), space.Kind) {
			cause = ""
		}
	}
	if cause == "" {
		return false, nil
	}
	if l.Changes[historyID(c)] != nil {
		return true, nil
	}
	c, speakerCtx := p.fillSender(ctx, c, l.IdentityDomainID)
	speaker := p.opts.Principals.Speaker(speakerCtx, historyUser(c), l.IdentityDomainID)
	added := false
	err = p.update(func(current *ledger.Ledger) {
		added = RetainHistorySkip(current, p.opts.Config, c, p.opts.Now(), speaker, catchUp, p.identity())
	})
	if err == nil && added && cause == "history_time_missing" {
		p.opts.Logger.Warn("inbound act has no usable history time", "space", c.Space, "kind", c.Kind)
	}
	return true, err
}

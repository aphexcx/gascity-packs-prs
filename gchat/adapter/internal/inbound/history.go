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

// Use the selected fallback in message revision IDs as well as in the bound.
// An absent resource time must not consume a later act with another publish time.
func historyTime(c puller.Change) puller.Change {
	if c.Message == nil || c.ActTime().IsZero() {
		return c
	}
	raw := c.Message.CreateTime
	if c.Kind == "message.updated" {
		raw = c.Message.LastUpdateTime
	}
	if _, err := time.Parse(time.RFC3339Nano, raw); err == nil {
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
func RetireQueuedHistory(l *ledger.Ledger, c puller.Change) {
	if HistorySkipCause(l, c) == "" {
		return
	}
	id := ""
	if c.Message != nil {
		id = c.Message.Name
		switch c.Kind {
		case "message.updated":
			base, state := l.Changes[id], l.Messages[id]
			if base == nil || base.Kind != c.Kind || state == nil || !state.Revision.Equal(c.ActTime()) {
				id += "#updated#" + c.Message.LastUpdateTime
			}
		case "message.deleted":
			id += "#deleted"
		}
	} else if c.Reaction != nil {
		id = c.Reaction.Name + "#" + strings.TrimPrefix(c.Kind, "reaction.")
	}
	if old := l.Changes[id]; old != nil && old.Kind == c.Kind && old.Status != "committed" {
		delete(l.Changes, id)
		delete(l.Unresolved, id)
		if c.Kind == "message.deleted" {
			if state := l.Messages[c.Message.Name]; state != nil {
				state.Deleted = false
			}
		}
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
	if cause == "" {
		return false, nil
	}
	if l.Changes[historyID(c)] != nil {
		return true, nil
	}
	speaker := p.opts.Principals.Speaker(ctx, historyUser(c), l.IdentityDomainID)
	added := false
	err = p.update(func(current *ledger.Ledger) {
		added = RetainHistorySkip(current, p.opts.Config, c, p.opts.Now(), speaker, catchUp, p.identity())
	})
	if err == nil && added && cause == "history_time_missing" {
		p.opts.Logger.Warn("inbound act has no usable history time", "space", c.Space, "kind", c.Kind)
	}
	return true, err
}

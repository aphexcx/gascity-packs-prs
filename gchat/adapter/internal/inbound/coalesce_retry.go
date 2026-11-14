package inbound

import (
	"encoding/json"
	"sort"
	"strings"
	"time"

	"gchat-adapter/internal/ledger"
	"gchat-adapter/internal/provenance"
	"gchat-adapter/internal/wire"
)

// refreshMerged runs inside the withdrawing act's save, and again before a
// retry for ledgers written by an older adapter. Pending receipts describe an
// already-started POST and are left for the normal receipt path.
func (p *Processor) refreshMerged(l *ledger.Ledger, owner *ledger.ChangeState) bool {
	if owner.Status != "intent" || len(owner.Members) == 0 {
		return false
	}
	changed := false
	var survivors, removed []*ledger.ChangeState
	for _, id := range owner.Members {
		member := l.Changes[id]
		if member == nil {
			markUnresolved(l, owner.ID, owner, "stored turn is malformed", p.opts.Now())
			return true
		}
		copy := *member
		copy.Event, copy.Kind = member.OriginalEvent(), member.OriginalKind()
		copy.MemberEvent, copy.MemberKind, copy.MemberOf, copy.Members = "", "", "", nil
		if superseded(l, member) {
			changed = true
			state := l.Messages[member.Message]
			if state.Deleted {
				removed = append(removed, member)
				continue
			}
			latest := currentEdit(l, member.Message, state.Revision)
			var w wire.InboundMessage
			if latest == nil || json.Unmarshal([]byte(latest.OriginalEvent()), &w) != nil {
				markUnresolved(l, owner.ID, owner, "stored turn is malformed", p.opts.Now())
				return true
			}
			w.ProviderMessageID = member.ID
			w.Text = strings.TrimPrefix(w.Text, "[catch-up]\n")
			if member.OriginalKind() == "message.created" {
				// Preserve a person's literal prefix: only an independently
				// rendered edit has the adapter's extra "edited: " prefix.
				fold := l.Changes[latest.MemberOf]
				if fold == nil || fold.Message != latest.Message || fold.OriginalKind() != "message.created" {
					frame, body, ok := strings.Cut(w.Text, "\n")
					if ok {
						w.Text = frame + "\n" + strings.TrimPrefix(body, "edited: ")
					}
				}
			}
			// Prefix removal can expose a body frame that Render did not see
			// at line start. Persist safe text even for a singleton survivor.
			w.Text = provenance.QuoteMergedHeaders(w.Text)
			if member.CatchUp {
				w.Text = "[catch-up]\n" + w.Text
			}
			raw, _ := json.Marshal(w)
			copy.Event, copy.MemberRevision = string(raw), state.Revision
		}
		survivors = append(survivors, &copy)
	}
	if !changed {
		return false
	}
	if len(survivors) == 0 {
		// One frozen delivery becomes one unresolved obligation. Its members
		// stay attached; even a released reader cannot replay its old payload.
		retireWithdrawn(l, owner, p.opts.Now())
		return true
	}
	sort.Slice(survivors, func(i, j int) bool {
		if survivors[i].ActAt.Equal(survivors[j].ActAt) {
			return survivors[i].ID < survivors[j].ID
		}
		return survivors[i].ActAt.Before(survivors[j].ActAt)
	})
	anchor := survivors[len(survivors)-1].ID
	for _, c := range survivors {
		if c.ID == owner.ID {
			anchor = owner.ID
		}
	}
	w, _, err := p.merged(survivors, anchor)
	if err != nil {
		markUnresolved(l, owner.ID, owner, "stored turn is malformed", p.opts.Now())
		return true
	}
	raw, _ := json.Marshal(w)
	// Retirement mutates the old owner. Capture the turn's delivery state
	// first, so changing its key cannot erase an ambiguous POST or a bound.
	delivery := *owner
	for _, c := range removed {
		c.MemberOf, c.Members = "", nil
		c.HeldUntil = time.Time{}
		retireWithdrawn(l, c, p.opts.Now())
	}
	ids := make([]string, 0, len(survivors))
	for _, copy := range survivors {
		ids = append(ids, copy.ID)
		c := l.Changes[copy.ID]
		c.Event, c.Kind, c.MemberRevision = copy.Event, copy.Kind, copy.MemberRevision
		c.MemberEvent, c.MemberKind, c.MemberOf, c.Members = "", "", anchor, nil
		c.HeldUntil = time.Time{}
		if c.ID == anchor {
			c.MemberEvent, c.MemberKind = copy.Event, copy.Kind
			c.Event, c.Kind, c.MemberOf = string(raw), "coalesced", ""
			c.Status, c.Created = "intent", delivery.Created
			if anchor != owner.ID {
				c.Attempts, c.Fragment = delivery.Attempts, delivery.Fragment
				c.Reason, c.Cause = delivery.Reason, delivery.Cause
				c.NextAttempt, c.Posted = delivery.NextAttempt, delivery.Posted
			}
		} else {
			c.Status, c.Reason = "committed", "coalesced"
		}
	}
	l.Changes[anchor].Members = ids
	return true
}

// Keep per-message history, but no replayable delivery bytes. Both current and
// released resolve handlers reject the empty Event before mutating retirement.
func retireWithdrawn(l *ledger.Ledger, c *ledger.ChangeState, at time.Time) {
	c.MemberEvent, c.MemberKind = c.OriginalEvent(), c.OriginalKind()
	c.Event, c.Kind = "", c.MemberKind
	markUnresolved(l, c.ID, c, reasonWithdrawn, at)
}

// invalidateWithdrawnCopies leaves the delivery and its diagnostics intact,
// but removes frozen payloads that a resolve handler or an older reader could
// POST. Run before intent refresh changes the shared member snapshots. The
// shape matters even after coalescing is disabled; ordinary singles stay as-is.
func invalidateWithdrawnCopies(l *ledger.Ledger, space string) bool {
	changed := false
	for _, records := range []map[string]*ledger.ChangeState{l.Changes, l.Unresolved} {
		for _, c := range records {
			if (space != "" && c.Space != space) || c.Event == "" || (c.Status != "pending" && c.Status != "unresolved") {
				continue
			}
			if len(c.Members) == 0 && c.MemberEvent == "" && c.Kind != "coalesced" {
				continue
			}
			withdrawn := superseded(l, c)
			for _, id := range c.Members {
				if member := l.Changes[id]; member != nil && superseded(l, member) {
					withdrawn = true
					break
				}
			}
			if !withdrawn {
				continue
			}
			c.MemberEvent, c.MemberKind = c.OriginalEvent(), c.OriginalKind()
			c.Event = ""
			changed = true
		}
	}
	return changed
}

func currentEdit(l *ledger.Ledger, message string, revision time.Time) *ledger.ChangeState {
	for _, c := range l.Changes {
		if c.Message != message || c.OriginalKind() != "message.updated" || c.Cause == "before_history_floor" || c.Cause == "history_time_missing" {
			continue
		}
		stamp, ok := strings.CutPrefix(c.ID, message+"#updated#")
		at, err := time.Parse(time.RFC3339Nano, stamp)
		if ok && err == nil && at.Equal(revision) {
			return c
		}
	}
	return nil
}

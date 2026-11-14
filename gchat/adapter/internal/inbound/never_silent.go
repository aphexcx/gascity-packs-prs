package inbound

import (
	"strings"
	"time"

	"gchat-adapter/internal/ledger"
	"gchat-adapter/internal/neversilent"
)

// NoticeDeliveryIDs returns a turn's owner and distinct members. The caller
// holds the ledger lock or supplies an immutable snapshot. It does not enroll
// anyone: Result and Stop ignore unknown IDs, and members use ForgetTracked.
func NoticeDeliveryIDs(l *ledger.Ledger, id string) []string {
	ids := []string{id}
	seen := map[string]bool{id: true}
	if c := l.Changes[id]; c != nil {
		for _, member := range c.Members {
			if member != "" && !seen[member] {
				ids = append(ids, member)
				seen[member] = true
			}
		}
	}
	return ids
}

func (p *Processor) observeNotice(id string, at time.Time, observe func(string, time.Time) error) error {
	if !p.opts.NeverSilent.Enabled() {
		return nil
	}
	p.opts.LedgerMu.Lock()
	ids := NoticeDeliveryIDs(p.opts.Ledger, id)
	p.opts.LedgerMu.Unlock()
	// A group shares one observation clock. A single keeps the engine's own
	// clock, exactly as Result and Stop did before coalescing.
	if at.IsZero() && len(ids) > 1 {
		at = p.opts.Now()
	}
	for _, member := range ids {
		if err := observe(member, at); err != nil {
			return err
		}
	}
	return nil
}

func (p *Processor) noticeResult(id, kind string, targets []string, at time.Time) error {
	return p.observeNotice(id, at, func(member string, at time.Time) error {
		return p.opts.NeverSilent.ResultAt(member, kind, targets, at)
	})
}

func (p *Processor) noticeStop(id string, definite bool) error {
	return p.observeNotice(id, time.Time{}, func(member string, at time.Time) error {
		return p.opts.NeverSilent.StopAt(member, definite, at)
	})
}

func (p *Processor) noticeForget(id string) error {
	return p.observeNotice(id, time.Time{}, func(member string, _ time.Time) error {
		if member == id {
			return p.opts.NeverSilent.Forget(member)
		}
		return p.opts.NeverSilent.ForgetTracked(member)
	})
}

// ReconcileNeverSilent repairs sidecar observations from a local ledger
// snapshot. It neither enrolls messages nor performs network I/O, so it can run
// before registration and while a credential generation is held.
func ReconcileNeverSilent(l *ledger.Ledger, notices *neversilent.Engine) error {
	if !notices.Enabled() {
		return nil
	}
	for _, send := range l.Sends {
		if send.Request == nil || send.Request.SessionID == "" {
			continue
		}
		for _, part := range send.Parts {
			if part.MessageName != "" && strings.TrimSpace(part.Text) != "" && !part.SentAt.IsZero() {
				if err := notices.Act(send.Space, "", send.Request.SessionID, part.SentAt); err != nil {
					return err
				}
			}
		}
	}
	for id, c := range l.Changes {
		member := c.MemberOf != ""
		resolved := c.NoticeResolved || c.Reason == "operator accepted" || c.Reason == "operator replay" || c.Reason == reasonWithdrawn
		retired := c.Status == "committed" && c.Reason == "local" && (c.Cause == "before_history_floor" || c.Cause == "history_time_missing")
		// Own retirement/resolution wins even after another member becomes
		// owner. Otherwise the delivery's durable outcome belongs to this ID too.
		if member && !resolved && !retired {
			c = l.Delivery(c)
		}
		var err error
		switch {
		case c.Status == "committed" && c.Reason == "local" && (c.Cause == "before_history_floor" || c.Cause == "history_time_missing"):
			// Migration can retain old delivery observations, but the retired
			// attempt no longer owes an answer. Do this before NoticeDelivered.
			err = notices.ForgetTracked(id)
			if err == nil && c.OriginalKind() == "message.created" && c.Message != id && l.Changes[c.Message] == nil {
				// A queued create with no usable time has a history-only ID.
				// Its old enrollment used the message ID. A surviving primary
				// record, or an enrollment newer than this skip (whose primary
				// save may have failed), belongs to a valid act.
				err = notices.ForgetTrackedBefore(c.Message, c.Created)
			}
		case resolved || c.NoticeResolved || c.Reason == "operator accepted" || c.Reason == "operator replay" || c.Reason == reasonWithdrawn:
			if member {
				err = notices.ForgetTracked(id)
			} else {
				err = notices.Forget(id)
			}
		case c.NoticeDelivered:
			err = notices.ResultAt(id, "delivered", nil, c.NoticeAt)
		case c.Status == "unresolved":
			err = notices.StopAt(id, strings.HasPrefix(c.Reason, "gc rejected the POST:"), c.NoticeAt)
		case c.Status == "committed" && c.Reason == "no_route":
			// A merged turn's journal names both observations: the no_route
			// answer and its definite stop. Preserve both on a cold restart.
			// Ordinary single-message reconciliation remains unchanged.
			if len(c.Members) > 0 {
				err = notices.ResultAt(id, "no_route", nil, c.NoticeAt)
			}
			if err == nil {
				err = notices.StopAt(id, true, c.NoticeAt)
			}
		case c.Status == "committed" && (c.Reason == "delivered" || c.Reason == reasonLanded):
			err = notices.ResultAt(id, "delivered", nil, c.NoticeAt)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

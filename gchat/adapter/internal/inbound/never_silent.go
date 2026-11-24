package inbound

import (
	"strings"

	"gchat-adapter/internal/ledger"
	"gchat-adapter/internal/neversilent"
)

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
		var err error
		switch {
		case c.Status == "committed" && c.Reason == "local" && (c.Cause == "before_history_floor" || c.Cause == "history_time_missing"):
			// Migration can retain old delivery observations, but the retired
			// attempt no longer owes an answer. Do this before NoticeDelivered.
			err = notices.ForgetTracked(id)
			if err == nil && c.Kind == "message.created" && c.Message != id && l.Changes[c.Message] == nil {
				// A queued create with no usable time has a history-only ID.
				// Its old enrollment used the message ID. A surviving primary
				// record, or an enrollment newer than this skip (whose primary
				// save may have failed), belongs to a valid act.
				err = notices.ForgetTrackedBefore(c.Message, c.Created)
			}
		case c.NoticeResolved || c.Reason == "operator accepted" || c.Reason == "operator replay" || c.Reason == reasonWithdrawn:
			err = notices.Forget(id)
		case c.NoticeDelivered:
			err = notices.ResultAt(id, "delivered", nil, c.NoticeAt)
		case c.Status == "unresolved":
			err = notices.StopAt(id, strings.HasPrefix(c.Reason, "gc rejected the POST:"), c.NoticeAt)
		case c.Status == "committed" && c.Reason == "no_route":
			err = notices.StopAt(id, true, c.NoticeAt)
		case c.Status == "committed" && (c.Reason == "delivered" || c.Reason == reasonLanded):
			err = notices.ResultAt(id, "delivered", nil, c.NoticeAt)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

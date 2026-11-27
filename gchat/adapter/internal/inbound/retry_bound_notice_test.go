package inbound

import (
	"strings"
	"testing"
	"time"

	"gchat-adapter/internal/fakes/fakegc"
	"gchat-adapter/internal/puller"
)

// Exercise the stopped-city branch and notice scheduler from an empty ledger.
// A successful Raise hook stands for a mailed notice with its wake requested.
func TestUnansweredPostSameLifeRetryBoundNotice(t *testing.T) {
	for _, waiting := range []bool{true, false} {
		name := "without_waiting_notice"
		if waiting {
			name = "with_waiting_notice"
		}
		t.Run(name, func(t *testing.T) {
			rf := retrySetup(t)
			rf.gc.Configure(func(s *fakegc.Server) { s.CityNotRunning = true })
			out, _ := rf.p.Handle(t.Context(), event("example", "message.created", message()))
			if out != puller.Nack {
				t.Fatalf("stopped city returned %v", out)
			}

			if waiting {
				rf.clock.Add(2 * time.Minute)
				if err := rf.p.Poll(t.Context()); err == nil {
					t.Fatal("expected stopped-city error")
				}
				mails := rf.raises.list()
				if len(mails) != 1 || mails[0].Ended || !strings.Contains(mails[0].Body, "Retrying until") {
					t.Fatal("expected a current-life waiting notice naming its deadline")
				}
			}
			rf.clock.Set(retryStart.Add(24 * time.Hour))
			if err := rf.p.Pass(t.Context()); err != nil {
				t.Fatal(err)
			}
			end := rf.change(t, messageName)
			dueAtEnd := rf.p.mailDue(rf.l, &end, rf.clock.Now())
			if dueAtEnd == waiting {
				t.Fatal("unexpected eligibility before any end-notice try")
			}
			rf.poll(t)
			endOffers := 0
			for _, notice := range rf.raises.list() {
				if notice.Ended {
					endOffers++
				}
			}
			// Reload the current build's saved end and advance another hour.
			rf.restart(t)
			rf.clock.Add(time.Hour)
			rf.poll(t)

			c := rf.change(t, messageName)
			due := rf.p.mailDue(rf.l, &c, rf.clock.Now())
			t.Logf("waiting_notice=%t status=%s cause=%s reason=%q due_at_end=%t end_offers=%d end_mark=%t due_at_25h=%t",
				waiting, c.Status, c.Cause, c.Reason, dueAtEnd, endOffers, !c.RaisedEnd.IsZero(), due)
			if c.Status != "unresolved" || c.Cause != "unknown" || c.Reason != reasonRetryBound || due {
				t.Fatal("unexpected terminal state")
			}
			if len(rf.raises.list()) != 0 {
				t.Fatal("unexpected offer after restart")
			}
			if waiting && (!c.RaisedEnd.IsZero() || endOffers != 0) {
				t.Fatal("waiting notice did not suppress the retry-bound end")
			}
			if !waiting && (c.RaisedEnd.IsZero() || endOffers != 1) {
				t.Fatal("control did not get its end notice")
			}
		})
	}
}

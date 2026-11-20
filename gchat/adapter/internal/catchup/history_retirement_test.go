package catchup

import (
	"testing"
	"time"

	"gchat-adapter/internal/ledger"
)

func TestRunningSweepCountsRetiredDeliveries(t *testing.T) {
	for _, kind := range []string{"message.created", "message.updated", "message.deleted", "reaction.created", "reaction.deleted"} {
		t.Run(kind, func(t *testing.T) {
			f := setup(t)
			l := f.opts.Ledger
			l.SpacesListedAt = frozen.Add(-time.Minute)
			f.addSpace("spaces/dm", false)
			l.Spaces["spaces/dm"] = &ledger.SpaceState{Kind: "dm", Wake: "all"}
			for _, status := range []string{"intent", "pending", "unresolved"} {
				id := "spaces/dm/messages/" + status
				created := frozen.Add(-time.Hour)
				if status == "unresolved" {
					created = time.Time{}
				}
				c := &ledger.ChangeState{ID: id, Message: id, Space: "spaces/dm", Kind: kind, Status: status, Created: created}
				l.Changes[id] = c
				if status == "unresolved" {
					l.Unresolved[id] = c
				}
			}
			for range 2 {
				f.freeze(t)
				if got := l.Counters["history_delivery_retired:spaces/dm"]; got != 3 {
					t.Errorf("retired counter=%d want 3", got)
				}
				if got := l.Counters["history_time_missing:spaces/dm"]; got != 1 {
					t.Errorf("missing-time counter=%d want 1", got)
				}
			}
			if err := f.p.Pass(t.Context()); err != nil {
				t.Fatal(err)
			}
			if len(f.gc.Inbound()) != 0 || len(l.Unresolved) != 0 {
				t.Error("retired attempt remained deliverable")
			}
		})
	}
}

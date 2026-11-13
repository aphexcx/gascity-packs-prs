package inbound

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"cloud.google.com/go/pubsub/v2"
	"gchat-adapter/internal/ledger"
	"gchat-adapter/internal/puller"
	api "google.golang.org/api/chat/v1"
)

// Cover the class at the decoded event boundary: neither delivery metadata nor
// a malformed resource timestamp may replace a message's own act time.
func TestMessageTimeIsItsOwnOrItsEventTime(t *testing.T) {
	const old = "2026-10-22T11:00:00Z"
	const recent = "2026-10-22T13:00:00Z"
	for _, kind := range []string{"created", "updated"} {
		for _, tc := range []struct {
			name, resource, event, publish string
			batch, unbounded               bool
			want                           int
			cause, quarantine              string
		}{
			{name: "old_event_recent_publish", event: old, publish: recent, cause: "before_history_floor"},
			{name: "batch_no_event", publish: recent, batch: true, cause: "history_time_missing"},
			{name: "catchup_recent_event", event: recent, want: 1},
			{name: "live_recent_event", event: recent, publish: recent, want: 1},
			{name: "missing_time", publish: recent, cause: "history_time_missing"},
			{name: "invalid_event_time", event: "not-a-time", publish: recent, cause: "history_time_missing"},
			{name: "no_time_no_publish", cause: "history_time_missing"},
			{name: "unbounded_no_time", publish: recent, unbounded: true, want: 1},
			{name: "unbounded_invalid_event", event: "not-a-time", publish: recent, unbounded: true, want: 1},
			{name: "unbounded_valid_event", event: recent, publish: recent, unbounded: true, want: 1},
			{name: "invalid_resource", resource: "not-a-time", event: recent, publish: recent, quarantine: "inbound: invalid message timestamp"},
			{name: "unbounded_invalid_resource", resource: "not-a-time", event: recent, publish: recent, unbounded: true, quarantine: "inbound: invalid message timestamp"},
			{name: "resource_old_event_recent", resource: old, event: recent, publish: recent, cause: "before_history_floor"},
			{name: "resource_recent_event_old", resource: recent, event: old, publish: old, want: 1},
			{name: "batch_resource_old_event_recent", resource: old, event: recent, publish: recent, batch: true, cause: "before_history_floor"},
			{name: "batch_resource_recent_event_old", resource: recent, event: old, publish: old, batch: true, want: 1},
		} {
			t.Run(kind+"/"+tc.name, func(t *testing.T) {
				f := setup(t)
				floor := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
				f.l.Spaces[space] = &ledger.SpaceState{Kind: "dm", ChatType: "DIRECT_MESSAGE", Wake: "all", MessageCursor: floor}
				if !tc.unbounded {
					f.l.HistoryFloor = floor
				}
				m := message()
				m.Space = &api.Space{Name: space, SpaceType: "DIRECT_MESSAGE"}
				m.CreateTime = ""
				if kind == "created" {
					m.CreateTime = tc.resource
				} else {
					m.LastUpdateTime = tc.resource
				}
				want, cause, quarantine := tc.want, tc.cause, tc.quarantine
				if kind == "updated" && tc.resource == "" && (tc.unbounded || tc.event == "" || tc.event == "not-a-time") {
					want, cause, quarantine = 0, "", "inbound: update time missing"
				}
				body := map[string]any{"message": m}
				action := kind
				if tc.batch {
					body = map[string]any{"messages": []any{body}}
					if kind == "created" {
						action = "batchCreated"
					} else {
						action = "batchUpdated"
					}
				}
				raw, err := json.Marshal(body)
				if err != nil {
					t.Fatal(err)
				}
				published, _ := time.Parse(time.RFC3339Nano, tc.publish)
				ev, err := puller.Decode(&pubsub.Message{Data: raw, PublishTime: published, Attributes: map[string]string{
					"ce-type": "google.workspace.chat.message.v1." + action, "ce-time": tc.event,
				}})
				if err != nil {
					t.Fatal(err)
				}
				out, err := f.p.Handle(context.Background(), ev)
				if quarantine != "" {
					if out != puller.Quarantine || err == nil || err.Error() != quarantine {
						t.Errorf("outcome=%v err=%v; want quarantine %q", out, err, quarantine)
					}
				} else if out != puller.Ack || err != nil {
					t.Errorf("outcome=%v err=%v; want ack", out, err)
				}
				if got := len(f.gc.Inbound()); got != want {
					t.Errorf("turns=%d want=%d", got, want)
				}
				missing := int64(0)
				if cause == "history_time_missing" {
					missing = 1
				}
				if got := f.l.Counters["history_time_missing:"+space]; got != missing {
					t.Errorf("missing-time counter=%d want=%d", got, missing)
				}
				if cause != "" {
					found := false
					for _, record := range f.l.Changes {
						found = found || (record.Message == m.Name && record.Cause == cause && record.Status == "committed")
					}
					if !found {
						t.Errorf("no committed history record with cause %q", cause)
					}
				}
			})
		}
	}
}

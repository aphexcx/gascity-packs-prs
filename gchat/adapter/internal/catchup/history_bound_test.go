package catchup

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"gchat-adapter/internal/ledger"
	"gchat-adapter/internal/puller"
	"google.golang.org/api/googleapi"
)

func patchHistory(t *testing.T, value any, fields map[string]any) {
	t.Helper()
	raw, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, value); err != nil {
		t.Fatal(err)
	}
}

func TestCursorlessSpaceStartsWhereItWasFirstSeen(t *testing.T) {
	for _, mode := range []string{"unreadable", "no-interval", "next-listing", "queued", "old-floor", "outage-dm"} {
		t.Run(mode, func(t *testing.T) {
			f := setup(t)
			start := frozen.Add(-time.Hour)
			patchHistory(t, f.opts.Ledger, map[string]any{"spaces_listed_at": start})
			f.addSpace("spaces/dm", false)
			f.google.Spaces["spaces/dm"].SpaceType = "DIRECT_MESSAGE"
			for i := range 120 {
				f.addMessage("spaces/dm", i, start.Add(-time.Duration(281+i)*24*time.Hour))
			}
			if mode != "next-listing" && mode != "outage-dm" {
				f.opts.Ledger.Spaces["spaces/dm"] = &ledger.SpaceState{Kind: "dm", Wake: "all"}
			}
			if mode == "old-floor" {
				f.opts.Ledger.HistoryFloor = frozen.Add(-40 * 24 * time.Hour)
				for i := range 31 {
					f.addMessage("spaces/dm", 200+i, frozen.Add(-time.Duration(i+1)*24*time.Hour))
				}
			}
			if mode == "queued" {
				s := f.opts.Ledger.Spaces["spaces/dm"]
				s.FrozenUpper = start.Add(-time.Minute)
				s.PageToken = encode(progress{Version: 1, Phase: "done", MessageLower: time.Unix(0, 0).UTC(), EventLower: time.Unix(0, 0).UTC(), Token: "stale"})
				for i := range 3 {
					m := f.google.Messages[fmt.Sprintf("spaces/dm/messages/m%04d", i)]
					at, _ := time.Parse(time.RFC3339Nano, m.CreateTime)
					s.Backlog = append(s.Backlog, encode(queuedChange{Change: puller.Change{Kind: "message.created", Space: "spaces/dm", Message: m}, At: at}))
				}
			}
			if mode == "unreadable" {
				refuse := true
				f.onRequest = func(r *http.Request) error {
					if refuse && r.URL.Path == "/v1/spaces/dm/messages" {
						return &googleapi.Error{Code: 403, Message: "permission denied"}
					}
					return nil
				}
				// Start and retain the interval while a listed space is unavailable.
				f.freeze(t)
				_, _, _ = f.r.Pass(t.Context())
				refuse = false
				s := f.opts.Ledger.Spaces["spaces/dm"]
				p, err := decodeProgress(s.PageToken)
				if err != nil {
					t.Fatal(err)
				}
				p.AccessRetryAfter = frozen
				s.PageToken = encode(p)
			}
			f.addMessage("spaces/dm", 999, start.Add(time.Minute))
			f.freeze(t)
			for range 5 {
				_, _, err := f.r.Pass(t.Context())
				if err != nil {
					t.Fatal(err)
				}
			}
			turns := f.gc.Inbound()
			t.Logf("turns=%d", len(turns))
			if len(turns) != 1 || turns[0].ProviderMessageID != "spaces/dm/messages/m0999" {
				t.Errorf("cursorless replay: %d turns", len(turns))
			}
			for _, m := range turns {
				if !strings.HasPrefix(m.Text, "[catch-up]") {
					t.Error("outage lost catch-up marker")
				}
			}
			if len(f.opts.Ledger.Spaces["spaces/dm"].Backlog) != 0 {
				t.Error("old queue retained")
			}
		})
	}
}

func TestNeverReadSpaceReportsNoLoss(t *testing.T) {
	for _, started := range []bool{false, true} {
		t.Run(fmt.Sprintf("start=%t", started), func(t *testing.T) {
			f := setup(t)
			old := frozen.Add(-40 * 24 * time.Hour)
			f.opts.Ledger.HistoryFloor = old
			f.addSpace("spaces/room", false)
			if started {
				s := &ledger.SpaceState{Kind: "room", MessageCursor: old, EventCursor: old}
				patchHistory(t, s, map[string]any{"history_start": old})
				f.opts.Ledger.Spaces["spaces/room"] = s
			}
			f.freeze(t)
			f.pass(t, 0, 0)
			if f.opts.Ledger.Counters["unrecoverable:spaces/room"] != 0 || strings.Contains(f.logs.String(), "catch-up history unrecoverable") {
				t.Error("never-read space reported lost history")
			}
		})
	}
}

func TestSavedCursorStillDeliversItsOutage(t *testing.T) {
	f := setup(t)
	f.addSpace("spaces/room", true)
	for i := range 3 {
		f.addMessage("spaces/room", i, frozen.Add(-30*time.Minute+time.Duration(i)*time.Second))
	}
	f.freeze(t)
	f.pass(t, 3, 0)
	f.freeze(t)
	f.pass(t, 0, 0)
}

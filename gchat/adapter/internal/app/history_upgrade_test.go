package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gchat-adapter/internal/fakes/fakegoogle"
	"gchat-adapter/internal/ledger"
	"gchat-adapter/internal/puller"
	api "google.golang.org/api/chat/v1"
)

func TestReleasedCursorlessHistoryCannotReplayAfterHold(t *testing.T) {
	f := newFixture(t, false)
	f.replaceIdentityToken(t, fakegoogle.UserID)
	raw, err := os.ReadFile("../ledger/testdata/ledger-0.5.0.json")
	if err != nil {
		t.Fatal(err)
	}
	var l ledger.Ledger
	if err := json.Unmarshal(raw, &l); err != nil {
		t.Fatal(err)
	}
	l.IdentityUserID, l.IdentitySeed = fakegoogle.UserID, fakegoogle.UserID
	l.Changes = map[string]*ledger.ChangeState{}
	l.Unresolved = map[string]*ledger.ChangeState{}
	l.Messages = map[string]*ledger.MessageState{}
	l.Counters = map[string]int64{}
	now := time.Now().UTC()
	cursor := now.Add(-time.Hour)
	p, _ := json.Marshal(map[string]any{"version": 1, "phase": "messages", "token": "expired", "message_lower": time.Unix(0, 0).UTC(), "event_lower": time.Unix(0, 0).UTC()})
	l.Spaces = map[string]*ledger.SpaceState{"spaces/dm": {Kind: "dm", Wake: "all", FrozenUpper: cursor, PageToken: string(p)}, "spaces/room": {Kind: "room", Wake: "all", MessageCursor: cursor, EventCursor: cursor}}
	f.google.Configure(func(g *fakegoogle.Server) {
		g.Spaces["spaces/dm"] = &api.Space{Name: "spaces/dm", SpaceType: "DIRECT_MESSAGE"}
		g.Spaces["spaces/room"] = &api.Space{Name: "spaces/room", SpaceType: "SPACE"}
		for i := range 120 {
			m := historyMessage("spaces/dm", fmt.Sprintf("old-%03d", i), now.Add(-time.Duration(281+i)*24*time.Hour))
			g.Messages[m.Name] = m
			if i < 3 {
				row, _ := json.Marshal(map[string]any{"change": puller.Change{Kind: "message.created", Space: "spaces/dm", Message: m}, "at": m.CreateTime})
				l.Spaces["spaces/dm"].Backlog = append(l.Spaces["spaces/dm"].Backlog, string(row))
			}
		}
		for i := range 3 {
			m := historyMessage("spaces/room", fmt.Sprintf("outage-%d", i), cursor.Add(time.Duration(i+1)*time.Minute))
			g.Messages[m.Name] = m
		}
	})
	if err := l.Save(f.cfg.Endpoints.StateFile); err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(f.cfg.Endpoints.ChatAPIBase)
	proxy := httputil.NewSingleHostReverseProxy(u)
	var refused atomic.Bool
	refused.Store(true)
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if refused.Load() && r.URL.Path == "/v1/spaces/dm/messages" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(403)
			_, _ = w.Write([]byte(`{"error":{"code":403,"message":"permission denied","status":"PERMISSION_DENIED"}}`))
			return
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(s.Close)
	f.cfg.Endpoints.ChatAPIBase = s.URL
	// Start without waiting for ready: one space is deliberately held.
	var notices atomic.Int32
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- RunWithOptions(ctx, f.cfg, Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Notify: func(context.Context, string, string) error { notices.Add(1); return nil }, RetryDelay: time.Millisecond})
	}()
	stopped := false
	stop := func() {
		if !stopped {
			stopped = true
			cancel()
			if err := <-done; err != nil {
				t.Error(err)
			}
			f.client.CloseIdleConnections()
		}
	}
	t.Cleanup(stop)
	waitFor(t, 15*time.Second, func() bool {
		loaded, _ := historyState(t, f)
		room := loaded.Spaces["spaces/room"]
		return loaded.Counters["unavailable:spaces/dm"] > 0 && len(f.gc.Inbound()) == 3 &&
			room.FrozenUpper.IsZero() && room.MessageCursor.After(cursor)
	})
	stop()
	loaded, _ := historyState(t, f)
	if !loaded.HistoryFloor.IsZero() {
		t.Error("released ledger gained a global floor")
	}
	start := loaded.Spaces["spaces/dm"].MessageCursor
	if start.Before(now) || loaded.Counters["history_backlog_skipped:spaces/dm"] != 3 || len(loaded.Spaces["spaces/dm"].Backlog) != 0 {
		t.Fatalf("migration start=%s skipped=%d backlog=%d", start, loaded.Counters["history_backlog_skipped:spaces/dm"], len(loaded.Spaces["spaces/dm"].Backlog))
	}
	for _, c := range loaded.Changes {
		if strings.HasPrefix(c.Message, "spaces/dm/") && (c.Status != "committed" || c.Reason != "local" || strings.Contains(c.Event, "old-")) {
			// Resource IDs may contain old-; inspect just the retained text below.
			var envelope struct{ Text string }
			_ = json.Unmarshal([]byte(c.Event), &envelope)
			if c.Status != "committed" || c.Reason != "local" || strings.Contains(envelope.Text, "\nold-") {
				t.Error("old queued body or pending delivery retained")
			}
		}
	}
	var progress map[string]any
	if err := json.Unmarshal([]byte(loaded.Spaces["spaces/dm"].PageToken), &progress); err != nil {
		t.Fatal(err)
	}
	progress["access_retry_after"] = time.Now().Add(-time.Second)
	b, _ := json.Marshal(progress)
	loaded.Spaces["spaces/dm"].PageToken = string(b)
	if err := loaded.Save(f.cfg.Endpoints.StateFile); err != nil {
		t.Fatal(err)
	}
	fresh := historyMessage("spaces/dm", "after-load", time.Now())
	f.google.Configure(func(g *fakegoogle.Server) { g.Messages[fresh.Name] = fresh })
	refused.Store(false)
	stop = boundStart(t, f, &notices)
	stop()
	turns := f.gc.Inbound()
	if len(turns) != 4 {
		t.Fatalf("outage plus new DM turns=%d want 4 (zero of 120 old)", len(turns))
	}
	seen := map[string]bool{}
	for _, m := range turns {
		if seen[m.ProviderMessageID] || !strings.HasPrefix(m.Text, "[catch-up]") || strings.Contains(m.ProviderMessageID, "/old-") {
			t.Error("duplicate, old, or unmarked turn")
		}
		seen[m.ProviderMessageID] = true
	}
	final, _ := historyState(t, f)
	if len(final.Unresolved) != 0 || notices.Load() != 0 {
		t.Errorf("notices=%d unresolved=%d", notices.Load(), len(final.Unresolved))
		for id, c := range final.Unresolved {
			t.Logf("unresolved %s: reason=%s cause=%s", id, c.Reason, c.Cause)
		}
	}
	t.Log("removed 3 old queued rows; delivered 3 saved-cursor outage messages and 1 new DM; zero of 120 old DM messages")
}

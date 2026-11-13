package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gchat-adapter/internal/config"
	"gchat-adapter/internal/fakes/fakegoogle"
	"gchat-adapter/internal/ledger"
	"gchat-adapter/internal/wire"
	api "google.golang.org/api/chat/v1"
)

func TestFirstStartCountsAndToldRetiredDeliveries(t *testing.T) {
	for _, mode := range []string{"one", "cursor", "two", "restore", "restore-cursor"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t, false)
			f.replaceIdentityToken(t, fakegoogle.UserID)
			before := time.Now().UTC().Add(-2 * time.Minute)
			l := ledger.New()
			l.IdentityUserID, l.IdentitySeed = fakegoogle.UserID, fakegoogle.UserID
			spaces := []string{"spaces/dm"}
			if mode == "two" {
				spaces = append(spaces, "spaces/other")
			}
			var references []string
			for _, space := range spaces {
				id := space + "/messages/one"
				references = append(references, id)
				s := &ledger.SpaceState{Kind: "dm", ChatType: "DIRECT_MESSAGE", Wake: "all"}
				if strings.Contains(mode, "cursor") {
					s.MessageCursor = before.Add(-time.Minute)
				}
				l.Spaces[space] = s
				w := wire.InboundMessage{ProviderMessageID: id,
					Conversation: wire.Conversation{ScopeID: "examplecity", Provider: "gchat", AccountID: "default", ConversationID: space, Kind: "dm"},
					Actor:        wire.Actor{ID: "users/222", DisplayName: "private-sender"}, Text: "private-message-text", ReceivedAt: before}
				raw, err := json.Marshal(w)
				if err != nil {
					t.Fatal(err)
				}
				l.Changes[id] = &ledger.ChangeState{ID: id, Message: id, Space: space, Kind: "message.created", Status: "intent", Created: before, Event: string(raw)}
				f.google.Configure(func(g *fakegoogle.Server) { g.Spaces[space] = &api.Space{Name: space, SpaceType: "DIRECT_MESSAGE"} })
			}
			path := f.cfg.Endpoints.StateFile
			if strings.HasPrefix(mode, "restore") {
				path = filepath.Join(t.TempDir(), "backup.json")
				writeFile(t, f.cfg.Endpoints.StateFile, "broken ledger")
			}
			if err := l.Save(path); err != nil {
				t.Fatal(err)
			}
			var mu sync.Mutex
			var mails []string
			f.startOptions(t, Options{Notify: func(_ context.Context, subject, body string) error {
				mu.Lock()
				defer mu.Unlock()
				mails = append(mails, subject+"\n"+body)
				return nil
			}})
			if strings.HasPrefix(mode, "restore") {
				f.health(t, "recovery-required", 5*time.Second)
				raw, _ := json.Marshal(map[string]string{"action": "restore", "file": path})
				resp, err := f.client.Post("http://adapter/state", "application/json", strings.NewReader(string(raw)))
				if err != nil {
					t.Fatal(err)
				}
				body, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				if resp.StatusCode != 200 {
					t.Fatalf("restore: %d %s", resp.StatusCode, body)
				}
			}
			f.health(t, "ready", 10*time.Second)
			wantTurns, wantCount := 0, int64(1)
			if strings.Contains(mode, "cursor") {
				wantTurns, wantCount = 1, 0
				waitFor(t, 5*time.Second, func() bool { return len(f.gc.Inbound()) == wantTurns })
			}
			if got := len(f.gc.Inbound()); got != wantTurns {
				t.Errorf("turns=%d want=%d", got, wantTurns)
			}
			loaded, _ := historyState(t, f)
			var counters map[string]int64
			if err := json.Unmarshal(f.status(t)["counters"], &counters); err != nil {
				t.Fatal(err)
			}
			for _, space := range spaces {
				key := "history_delivery_retired:" + space
				if loaded.Counters[key] != wantCount || counters[key] != wantCount {
					t.Errorf("%s: ledger=%d status=%d want=%d", key, loaded.Counters[key], counters[key], wantCount)
				}
			}
			wantMail := wantCount > 0 && !strings.HasPrefix(mode, "restore")
			// Let the service notification worker drain before checking exact counts.
			deadline := time.Now().Add(time.Second)
			for time.Now().Before(deadline) {
				mu.Lock()
				got := len(mails)
				mu.Unlock()
				if got > 0 || !wantMail {
					break
				}
				time.Sleep(5 * time.Millisecond)
			}
			mu.Lock()
			defer mu.Unlock()
			if !wantMail {
				if len(mails) != 0 {
					t.Errorf("no-mail start sent %d mails", len(mails))
				}
				return
			}
			if len(mails) != 1 {
				t.Fatalf("retired deliveries sent %d mails; want 1", len(mails))
			}
			t.Logf("retirement mail: %s", mails[0])
			for _, value := range append(references, fmt.Sprint(len(references))) {
				if !strings.Contains(mails[0], value) {
					t.Errorf("mail omits %q", value)
				}
			}
			if strings.Contains(mails[0], "private-message-text") || strings.Contains(mails[0], "private-sender") {
				t.Error("mail exposed message text or sender name")
			}
		})
	}
}

func TestHistoryRetirementNoticeDedup(t *testing.T) {
	a := &application{cfg: &config.Config{}, notices: make(chan notification, 8), notified: map[string]bool{}}
	for _, start := range []string{"first-start", "first-start", "second-start"} {
		a.notice("history-delivery-retired", "1 waiting delivery: spaces/dm/messages/one", start)
	}
	if len(a.notices) != 2 {
		t.Fatalf("start notifications=%d want 2", len(a.notices))
	}
	one, two := <-a.notices, <-a.notices
	if one.dedup == two.dedup {
		t.Error("different starts share a dedup key")
	}
	if one.message != two.message || strings.Contains(one.message, "Subscription reconciliation failed") {
		t.Error("retirement body changed or gained unrelated subscription advice")
	}
}

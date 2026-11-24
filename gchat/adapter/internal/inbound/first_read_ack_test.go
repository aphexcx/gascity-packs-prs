package inbound_test

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gchat-adapter/internal/ack"
	"gchat-adapter/internal/arrival"
	"gchat-adapter/internal/catchup"
	"gchat-adapter/internal/chat"
	"gchat-adapter/internal/config"
	"gchat-adapter/internal/fakes/fakegc"
	"gchat-adapter/internal/fakes/fakegoogle"
	"gchat-adapter/internal/gcclient"
	"gchat-adapter/internal/inbound"
	"gchat-adapter/internal/ledger"
	"gchat-adapter/internal/puller"
	"gchat-adapter/internal/stopwatch"
	api "google.golang.org/api/chat/v1"
)

func TestFirstReadAcknowledgementBoundary(t *testing.T) {
	for _, tc := range []struct {
		name             string
		live, belowFloor bool
	}{
		{name: "catchup-only"},
		{name: "live-before-first-read", live: true},
		{name: "live-before-history-floor", live: true, belowFloor: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("ACK_REACTION", "eyes")
			ctx := t.Context()
			now := time.Now().UTC()
			joined := now.Add(-time.Second)
			google := fakegoogle.New(t, nil)
			gc := fakegc.New(t)
			google.Now = func() time.Time { return now }
			space := &api.Space{Name: "spaces/joined", SpaceType: "SPACE"}
			message := &api.Message{
				Name: space.Name + "/messages/adding", Space: space,
				Text: "please join", CreateTime: joined.Add(-2 * time.Second).Format(time.RFC3339Nano),
				Sender:      &api.User{Name: "users/456", Type: "HUMAN"},
				Thread:      &api.Thread{Name: space.Name + "/threads/topic"},
				Annotations: []*api.Annotation{{Type: "USER_MENTION", UserMention: &api.UserMentionMetadata{User: &api.User{Name: "users/123"}}}},
			}
			google.Spaces[space.Name] = space
			google.Messages[message.Name] = message
			dir := t.TempDir()
			cfg := &config.Config{
				Secrets:         config.Secrets{CityName: "examplecity"},
				Endpoints:       config.Endpoints{ChatAPIBase: google.URL(), StateFile: filepath.Join(dir, "state.json")},
				Acknowledgement: ack.Options{Mode: os.Getenv("ACK_REACTION")},
			}
			cfg.Settings.Identities = []config.Identity{{Name: "example", Agent: "mayor"}}
			client, err := chat.New(ctx, http.DefaultClient, cfg.Endpoints)
			if err != nil {
				t.Fatal(err)
			}
			service, watch, err := arrival.New(arrival.Options{
				Acknowledgement: cfg.Acknowledgement, StateDir: dir, Chat: func() *chat.Client { return client },
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(watch.Close)
			arrival.Wire(client, service, watch)
			reacted := make(chan struct{}, 8)
			recordReaction := service.Reaction
			service.Reaction = func(in ack.Input, how string, at time.Time) {
				recordReaction(in, how, at)
				reacted <- struct{}{}
			}
			l, mu := ledger.New(), &sync.Mutex{}
			if tc.belowFloor {
				l.HistoryFloor = joined.Add(-time.Second)
			}
			processor, err := inbound.New(inbound.Options{
				Config: cfg, Ledger: l, LedgerMu: mu, LedgerPath: cfg.Endpoints.StateFile,
				GC: gcclient.New(gc.URL(), "examplecity"), Chat: client, IdentityUserID: "users/123",
				Ack: service, Now: func() time.Time { return now },
			})
			if err != nil {
				t.Fatal(err)
			}
			handle := func(event *puller.Event) {
				t.Helper()
				outcome, err := processor.Handle(ctx, event)
				if err != nil || outcome != puller.Ack {
					t.Fatalf("Handle = %v, %v", outcome, err)
				}
			}
			handle(&puller.Event{Changes: []puller.Change{{Kind: "membership.created", Space: space.Name, Membership: &api.Membership{
				Name: space.Name + "/members/123", State: "JOINED", CreateTime: joined.Format(time.RFC3339Nano),
				Member: &api.User{Name: "users/123", Type: "HUMAN"},
			}}}})
			if tc.live {
				handle(&puller.Event{PubSubID: "adding-live", Changes: []puller.Change{{Kind: "message.created", Space: space.Name, Message: message}}})
			}
			if tc.live && !tc.belowFloor {
				select {
				case <-reacted:
				case <-time.After(5 * time.Second):
					t.Fatal("live delivery did not acknowledge")
				}
				service.Wait()
			}
			runner, err := catchup.New(catchup.Options{
				Config: cfg, Ledger: l, LedgerMu: mu, LedgerPath: cfg.Endpoints.StateFile,
				Chat: client, Processor: processor, Now: func() time.Time { return now },
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := runner.Freeze(ctx, now); err != nil {
				t.Fatal(err)
			}
			if _, remaining, err := runner.Pass(ctx); err != nil || remaining != 0 {
				t.Fatalf("first read: remaining=%d err=%v", remaining, err)
			}
			wantTurns := 1
			if tc.belowFloor {
				wantTurns = 0
				if c := l.Changes[message.Name]; c == nil || c.Cause != "before_history_floor" {
					t.Fatal("missing history-floor skip")
				}
			}
			if got := len(gc.Inbound()); got != wantTurns {
				t.Fatalf("turns across live and first read = %d, want %d", got, wantTurns)
			}
			if !l.Spaces[space.Name].JoinedAt.Equal(joined) {
				t.Fatal("membership join time lost")
			}
			// Catch-up must never enter the asynchronous arrival path. Bound the
			// negative observation, then drain the service and stopwatch writer.
			select {
			case <-reacted:
				t.Errorf("unexpected acknowledgement during %s", tc.name)
			case <-time.After(250 * time.Millisecond):
			}
			service.Wait()
			watch.Flush()
			want := 0
			if tc.live && !tc.belowFloor {
				want = 1
			}
			google.Configure(func(s *fakegoogle.Server) {
				if got := len(s.Reactions); got != want {
					t.Errorf("acknowledgements = %d, want %d", got, want)
				}
			})
			files, err := filepath.Glob(filepath.Join(dir, "stopwatch-*.jsonl"))
			if err != nil {
				t.Fatal(err)
			}
			counts := map[string]int{}
			for _, file := range files {
				body, err := os.ReadFile(file)
				if err != nil {
					t.Fatal(err)
				}
				for _, line := range strings.Split(strings.TrimSpace(string(body)), "\n") {
					var event stopwatch.Event
					if err := json.Unmarshal([]byte(line), &event); err != nil {
						t.Fatal(err)
					}
					if event.Message == message.Name {
						counts[event.Event]++
					}
				}
			}
			if counts["inbound"] != want || counts["reaction"] != want || (want == 0 && len(counts) != 0) {
				t.Fatalf("stopwatch lines = %v, want inbound=%d reaction=%d", counts, want, want)
			}
		})
	}
}

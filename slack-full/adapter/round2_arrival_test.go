package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjarmak/gc-slack-adapter/internal/ack"
)

// syncBuffer collects log output written from several goroutines.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}
func (b *syncBuffer) String() string { b.mu.Lock(); defer b.mu.Unlock(); return b.buf.String() }

// The arrival task decides beside the delivery worker and never adds to its
// counter, its operator log line or its membership probe, switch on or off.
// On a cold member cache the worker starts the acknowledgement itself once it
// admits the wake, so the message still gets exactly one reaction.
func TestArrivalNeverCountsRosterDrift(t *testing.T) {
	for _, tc := range []struct {
		name, mode string
		warm       bool
	}{
		{"off cold", "", false}, {"off warm", "", true}, {"eyes cold", "eyes", false}, {"eyes warm", "eyes", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var reactions atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/reactions.add" {
					reactions.Add(1)
				}
				_, _ = io.WriteString(w, `{"ok":true}`)
			}))
			defer srv.Close()
			prev := slackAPIBase
			slackAPIBase = srv.URL
			defer func() { slackAPIBase = prev }()
			var logs syncBuffer
			log.SetOutput(&logs)
			defer log.SetOutput(os.Stderr)

			gc := newFakeGC(t)
			df := baseDirectoryFile()
			df.Rooms[0].MentionWake = []string{"ollie"}
			bf := baseBindingsFile()
			h := newCompanyHarness(t, gc.server.URL, &df, &bf, 4)
			setFixedClock(h)
			var probes int32
			h.gw.channelMembersProbe = countingMembersProbe(map[string]bool{botRiley: true}, true, &probes)
			h.gw.slackToken = "test-token"
			env := map[string]string{"ACK_REACTION": tc.mode, "GC_SERVICE_STATE_ROOT": t.TempDir()}
			if err := configureArrival(&h.gw.cfg, func(k string) string { return env[k] }); err != nil {
				t.Fatal(err)
			}
			inputs := make(chan ack.Input, 8)
			h.gw.cfg.arrivalStarted = time.Now().UTC()
			h.gw.cfg.arrival = &ack.Service{Choice: h.gw.cfg.arrivalChoice, Dir: h.gw.cfg.arrivalState, Inbound: func(in ack.Input) { inputs <- in }}
			if tc.warm {
				if _, ok := h.gw.liveChannelMembers(testChannelID); !ok {
					t.Fatal("warm the member cache")
				}
			}
			h.openBarrier()

			// A live message: the acknowledgement ignores one older than 60 s.
			now := time.Now()
			ts := fmt.Sprintf("%d.%06d", now.Unix(), now.Nanosecond()/1000)
			if _, handled := h.admitViaHandler(t, humanMessage(ts, driftMention(botRiley)), 0); !handled {
				t.Fatal("not handled")
			}
			h.wait()
			// The arrival tasks are detached; give them time to finish.
			deadline := time.Now().Add(2 * time.Second)
			for tc.mode != "" && reactions.Load() == 0 && time.Now().Before(deadline) {
				time.Sleep(10 * time.Millisecond)
			}
			time.Sleep(200 * time.Millisecond)
			h.gw.cfg.arrival.Wait()

			if got := h.gw.rosterDriftWakes.Load(); got != 1 {
				t.Errorf("company_roster_drift_wakes = %d, want 1 as at the base", got)
			}
			if n := strings.Count(logs.String(), "roster-drift eligibility overlay admitted"); n != 1 {
				t.Errorf("roster-drift log lines = %d, want 1 as at the base", n)
			}
			if n := atomic.LoadInt32(&probes); n > 1 {
				t.Errorf("membership probes = %d, want at most 1 as at the base", n)
			}
			if sc := gc.sessionCalls(); len(sc) != 1 {
				t.Errorf("session calls = %d, want 1", len(sc))
			}
			if len(inputs) == 0 {
				t.Errorf("the admitted mention has no stopwatch inbound observation")
			}
			want := int32(0)
			if tc.mode != "" {
				want = 1
			}
			if got := reactions.Load(); got != want {
				t.Errorf("reactions = %d, want %d", got, want)
			}
		})
	}
}

// The start reads ACK_SCOPE only when ACK_REACTION is set, as the base, which
// reads it nowhere, starts with any value.
func TestScopeUncheckedWhileOff(t *testing.T) {
	var logs syncBuffer
	log.SetOutput(&logs)
	defer log.SetOutput(os.Stderr)
	env := map[string]string{"ACK_SCOPE": "everyone", "GC_SERVICE_STATE_ROOT": t.TempDir()}
	var cfg config
	if err := configureArrival(&cfg, func(k string) string { return env[k] }); err != nil {
		t.Fatalf("switch off, ACK_SCOPE=everyone: start refused: %v", err)
	}
	if logs.String() != "" || cfg.arrivalChoice.Enabled() || cfg.arrivalChoice.All() {
		t.Fatalf("switch off: log=%q enabled=%t all=%t", logs.String(), cfg.arrivalChoice.Enabled(), cfg.arrivalChoice.All())
	}
	env["ACK_REACTION"] = "eyes"
	if err := configureArrival(&cfg, func(k string) string { return env[k] }); err == nil || err.Error() != "ACK_SCOPE must be addressed or all" {
		t.Fatalf("switch on, ACK_SCOPE=everyone: %v", err)
	}
}

// Slack already redacts the Jev state; this pins it as the class test the
// other two adapters now match, through the start's own wiring.
func TestJevStateIsRedactedLikeSlack(t *testing.T) {
	data, err := os.ReadFile("../tests/fixtures/redaction/cases.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Name, Text, Want string
		TextParts        []string `json:"text_parts"`
	}
	if err := json.Unmarshal(data, &cases); err != nil || len(cases) == 0 {
		t.Fatalf("fixtures: %d, %v", len(cases), err)
	}
	var mu sync.Mutex
	var states []string
	jev := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct{ State string }
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &request)
		mu.Lock()
		states = append(states, request.State)
		mu.Unlock()
		_, _ = io.WriteString(w, `{"answers":{"ack":{"choice":"option_001"}}}`)
	}))
	defer jev.Close()
	env := map[string]string{"ACK_REACTION": "jev", "JEV_API_KEY": "test-credential", "JEV_API_BASE_URL": jev.URL, "GC_SERVICE_STATE_ROOT": t.TempDir()}
	var cfg config
	if err := configureArrival(&cfg, func(k string) string { return env[k] }); err != nil {
		t.Fatal(err)
	}
	matched := 0
	for _, tc := range cases {
		text := tc.Text
		if tc.TextParts != nil {
			text = strings.Join(tc.TextParts, "")
		}
		want := []rune(tc.Want)
		if len(want) > 1000 {
			want = want[:1000]
		}
		mu.Lock()
		states = nil
		mu.Unlock()
		_, how := cfg.arrivalChoice.Choose(context.Background(), text, time.Now())
		mu.Lock()
		got := append([]string(nil), states...)
		mu.Unlock()
		if strings.TrimSpace(string(want)) == "" {
			if len(got) != 0 {
				t.Errorf("%s: blank text reached Jev", tc.Name)
			}
			continue
		}
		// Report by name and size only: the inputs are credential-shaped.
		if how != "jev" || len(got) != 1 || got[0] != string(want) {
			size := -1
			if len(got) == 1 {
				size = len([]rune(got[0]))
			}
			t.Errorf("%s: how=%s requests=%d state is not the redacted want (state %d chars, want %d)", tc.Name, how, len(got), size, len(want))
			continue
		}
		matched++
	}
	t.Logf("redacted like Slack: %d of %d shared cases", matched, len(cases))
}

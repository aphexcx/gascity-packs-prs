package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjarmak/gc-slack-adapter/internal/ack"
	"github.com/sjarmak/gc-slack-adapter/internal/stopwatch"
)

func captureArrivalInputs(t *testing.T, cfg *config) <-chan ack.Input {
	t.Helper()
	inputs := make(chan ack.Input, 4)
	choice, err := ack.New(ack.Options{Catalog: ack.Catalog})
	if err != nil {
		t.Fatal(err)
	}
	cfg.arrival = &ack.Service{Choice: choice, Dir: t.TempDir(), Inbound: func(in ack.Input) { inputs <- in }}
	return inputs
}

func requireArrivalInput(t *testing.T, inputs <-chan ack.Input, want bool) {
	t.Helper()
	select {
	case <-inputs:
		if !want {
			t.Fatal("routing rejected the message but arrival admitted it")
		}
	case <-time.After(100 * time.Millisecond):
		if want {
			t.Fatal("admitted turn has no arrival timing record")
		}
	}
}

func TestArrivalUsesDMFamilyAdmissionPolicy(t *testing.T) {
	for _, tc := range []struct {
		name                          string
		mpim, denied, mentioned, want bool
	}{
		{"denied DM", false, true, false, false},
		{"denied MPIM", true, true, true, false},
		{"unmentioned MPIM", true, false, false, false},
		{"allowed DM", false, false, false, true},
		{"mentioned MPIM", true, false, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _, _, _ := setupMpim(t)
			if tc.denied {
				df := baseDirectoryFile()
				df.DMAllowedHumans = &[]string{}
				body, _ := json.Marshal(df)
				dir, err := ParseCompanyDirectory(body)
				if err != nil {
					t.Fatal(err)
				}
				h.dirStore.set(dir)
			}
			inputs := captureArrivalInputs(t, &h.gw.cfg)
			text := "synthetic turn"
			if tc.mentioned {
				text = "<@U0AAAAAA1> " + text
			}
			ts := slackTimestamp(time.Now())
			if tc.mpim {
				admitMpimViaHandler(t, h, mpimMessage("U0HUMAN01", ts, text), ollieAppID, 0)
			} else {
				admitDMViaHandler(t, h, dmMessage("U0HUMAN01", ts, text), ollieAppID, 0)
			}
			h.wait()
			requireArrivalInput(t, inputs, tc.want)
		})
	}
}

func TestArrivalCoversCompanyAddressingRoutes(t *testing.T) {
	for _, route := range []string{"switchboard", "handle", "user group", "own thread", "thread broadcast"} {
		t.Run(route, func(t *testing.T) {
			gc := newFakeGC(t)
			df, bf := baseDirectoryFile(), baseBindingsFile()
			h := newCompanyHarness(t, gc.server.URL, &df, &bf, 4)
			h.openBarrier()
			withMentionOnlyLane(t, h, outsiderBinding())
			aliases := newTestHandleAliasRegistry(t)
			if err := aliases.Set("outsider", "outsider-session"); err != nil {
				t.Fatal(err)
			}
			h.gw.cfg.handleAliases = aliases
			subteams, err := newSubteamAliasMap(writeSubteamAliasFile(t, t.TempDir(), map[string]string{"S0OUTSIDER": "outsider"}))
			if err != nil {
				t.Fatal(err)
			}
			h.gw.cfg.subteamAliases = subteams
			inputs := captureArrivalInputs(t, &h.gw.cfg)
			now := time.Now()
			ev := humanMessage(slackTimestamp(now), "<@"+companyMOBotUserID+"> synthetic")
			switch route {
			case "handle":
				ev.Text = "@outsider: synthetic"
			case "user group":
				ev.Text = "<!subteam^S0OUTSIDER> synthetic"
			case "own thread":
				ev.Text, ev.ThreadTS = "synthetic follow-up", slackTimestamp(now.Add(-time.Second))
				h.gw.cfg.ownThreads.record(testChannelID, ev.ThreadTS, "outsider-session")
			case "thread broadcast":
				ev.Text, ev.Subtype = "<@U0AAAAAA1> synthetic", "thread_broadcast"
			}
			admitCompanyRoomMessage(t, h, ev)
			h.wait()
			requireArrivalInput(t, inputs, true)
		})
	}
}

func TestArrivalCoversLegacyThreadRoutes(t *testing.T) {
	for _, route := range []string{"sticky", "own thread"} {
		t.Run(route, func(t *testing.T) {
			slackStub, _ := newReactionRecordingSlackStub(t)
			withSlackAPIStub(t, slackStub)
			gc := &mentionOnlyGCStub{}
			server := httptest.NewServer(gc.handler())
			defer server.Close()
			cfg := mentionOnlyTestConfig(t, server.URL)
			inputs := captureArrivalInputs(t, &cfg)
			now := time.Now()
			root := slackTimestamp(now.Add(-time.Second))
			sticky := newThreadHandleStickiness()
			if route == "sticky" {
				sticky.Bind("C1", root, "mayor")
			} else {
				cfg.ownThreads.record("C1", root, "mayor-session")
			}
			env := plainRoomEnvelope(t, "event-example", "C1", slackTimestamp(now), root, "synthetic follow-up")
			processSlackEvent(cfg, newTestHandleAliasRegistry(t), nil, nil, nil, sticky, env, func() {})
			requireArrivalInput(t, inputs, true)
		})
	}
}

func TestArrivalExcludesRecoveredCompanyMentionOnlyTurn(t *testing.T) {
	for _, path := range []string{"startup recovery", "redelivery"} {
		t.Run(path, func(t *testing.T) {
			gc := newFakeGC(t)
			df, bf := baseDirectoryFile(), baseBindingsFile()
			h := newCompanyHarness(t, gc.server.URL, &df, &bf, 4)
			h.openBarrier()
			withMentionOnlyLane(t, h, outsiderBinding())
			h.gw.mentionOnlyLaneStart = func() bool { return false }
			ev := humanMessage(slackTimestamp(time.Now()), "<@"+companyMOBotUserID+"> synthetic turn")
			admitCompanyRoomMessage(t, h, ev)
			h.wait()
			// A restart has no arrival marker when the live lane never ran. Recovery
			// still delivers its owed reminder, but must not time or acknowledge it.
			inputs := captureArrivalInputs(t, &h.gw.cfg)
			h.gw.mentionOnlyLaneStart = nil
			h.gw.cfg.channelClaims = newEventDedupCache(time.Minute)
			if path == "startup recovery" {
				h.gw.replayMentionOnlyPending()
			} else {
				admitCompanyRoomMessage(t, h, ev)
			}
			h.wait()
			if outsiderPostCount(gc) != 1 {
				t.Fatal("recovery did not deliver its owed turn")
			}
			requireArrivalInput(t, inputs, false)
		})
	}
}

func TestStopwatchPublishToCanonicalThreadReport(t *testing.T) {
	fastReadbackTimings(t)
	for _, route := range []string{"fresh", "cached", "session act", "restart", "response thread"} {
		t.Run(route, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/chat.postMessage" {
					if route == "response thread" {
						fmt.Fprint(w, `{"ok":true,"ts":"1700000022.000001","message":{"thread_ts":"1700000000.000001"}}`)
					} else {
						fmt.Fprint(w, `{"ok":true,"ts":"1700000022.000001"}`)
					}
				} else {
					http.Error(w, "unavailable", http.StatusServiceUnavailable)
				}
			}))
			defer server.Close()
			withSlackAPIStub(t, server)
			dir := t.TempDir()
			watch := stopwatch.New(dir, "slack", "release-example", nil)
			watch.Record(stopwatch.Event{Event: "inbound", Conversation: "CEXAMPLE", Message: "1700000020.000001", Thread: "1700000000.000001", At: time.Unix(1700000020, 1000), MessageAt: time.Unix(1700000020, 1000)})
			if route == "restart" {
				watch = stopwatch.New(dir, "slack", "release-example", nil)
			}
			cfg := config{slackBotToken: "test-credential", inboundJournal: &inboundJournal{watch: watch}}
			if route == "session act" {
				cfg.inboundJournal.recordSessionAct(ingestionSessionAct{Kind: "reply", Source: "session_publish", Channel: "CEXAMPLE", MessageTS: "1700000022.000001", ThreadTS: "1700000020.000001", SessionID: "session-example", Timestamp: time.Unix(1700000022, 1000)})
			} else {
				dedup := newPublishDedupCache(publishDedupTTL)
				if route == "cached" {
					dedup.Put("request-example", publishReceipt{Delivered: true, MessageID: "1700000022.000001", readbackTarget: slackReadbackTarget{channel: "CEXAMPLE", threadTS: "1700000020.000001", messageTS: "1700000022.000001"}})
				}
				replyTo := "1700000020.000001"
				if route == "response thread" {
					replyTo = "1700000015.000001"
				}
				body := fmt.Sprintf(`{"idempotency_key":"request-example","session_id":"session-example","conversation":{"conversation_id":"CEXAMPLE"},"reply_to_message_id":%q,"text":"synthetic reply"}`, replyTo)
				w := httptest.NewRecorder()
				handlePublish(cfg, nil, nil, dedup)(w, httptest.NewRequest(http.MethodPost, "/publish", strings.NewReader(body)))
			}
			files, _ := filepath.Glob(filepath.Join(dir, "stopwatch-*.jsonl"))
			for _, file := range files {
				data, _ := os.ReadFile(file)
				for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
					var event stopwatch.Event
					if json.Unmarshal([]byte(line), &event) != nil {
						t.Fatal("invalid timing record")
					}
					if event.Event == "words" && event.Thread != "1700000000.000001" {
						t.Fatalf("words thread = %s, want canonical root", event.Thread)
					}
				}
			}
			output, err := exec.Command("python3", "../commands/stopwatch.py", "--state-dir", dir, "--json").CombinedOutput()
			if err != nil {
				t.Fatalf("report: %v: %s", err, output)
			}
			var reports []struct {
				Words struct {
					Count  int
					Median float64 `json:"median_seconds"`
				}
				Unanswered int `json:"without_words_over_hour"`
			}
			if json.Unmarshal(output, &reports) != nil || len(reports) != 1 || reports[0].Words.Count != 1 || reports[0].Words.Median != 2 || reports[0].Unanswered != 0 {
				t.Fatalf("accepted threaded reply was not measured: %s", output)
			}
		})
	}
}

func TestStopwatchAcceptedPublishWithoutReadback(t *testing.T) {
	fastReadbackTimings(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chat.postMessage" {
			fmt.Fprint(w, `{"ok":true,"ts":"1700000002.000001"}`)
		} else {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
		}
	}))
	defer server.Close()
	withSlackAPIStub(t, server)
	dir := t.TempDir()
	cfg := config{slackBotToken: "test-credential", inboundJournal: &inboundJournal{watch: stopwatch.New(dir, "slack", "release-example", nil)}}
	handler := handlePublish(cfg, nil, nil, newPublishDedupCache(publishDedupTTL))
	for i := 0; i < 2; i++ {
		w := httptest.NewRecorder()
		handler(w, httptest.NewRequest(http.MethodPost, "/publish", strings.NewReader(`{"idempotency_key":"request-example","session_id":"session-example","conversation":{"conversation_id":"CEXAMPLE"},"reply_to_message_id":"1700000000.000001","text":"synthetic reply"}`)))
		var receipt publishReceipt
		if json.Unmarshal(w.Body.Bytes(), &receipt) != nil || !receipt.Delivered {
			t.Fatalf("publish failed: %s", w.Body)
		}
		files, _ := filepath.Glob(filepath.Join(dir, "stopwatch-*.jsonl"))
		if len(files) != 1 {
			t.Fatal("Slack accepted the reply but stopwatch has no words")
		}
		body, _ := os.ReadFile(files[0])
		var event stopwatch.Event
		if json.Unmarshal(body, &event) != nil || event.Event != "words" || event.At.Unix() != 1700000002 {
			t.Fatal("accepted timestamp lost or repeated")
		}
	}
}

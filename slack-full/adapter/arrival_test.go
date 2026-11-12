package main

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/sjarmak/gc-slack-adapter/internal/ack"
	"github.com/sjarmak/gc-slack-adapter/internal/stopwatch"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestNineSlackArrivalsDeliverBesideHeldChoices(t *testing.T) {
	capture := &inboundCapture{}
	gc := httptest.NewServer(capture.handler())
	defer gc.Close()
	cfg := busyTestConfig(gc.URL)
	cfg.busyReaction = ""
	var active, maximum atomic.Int32
	stop := make(chan struct{})
	vendor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		n := active.Add(1)
		defer active.Add(-1)
		for old := maximum.Load(); n > old && !maximum.CompareAndSwap(old, n); old = maximum.Load() {
		}
		select {
		case <-stop:
		case <-r.Context().Done():
		}
	}))
	defer vendor.Close()
	defer close(stop)
	choice, err := ack.New(ack.Options{Mode: "jev", Key: "test-credential", Base: vendor.URL, Catalog: ack.Catalog})
	if err != nil {
		t.Fatal(err)
	}
	reactions := make(chan string, 9)
	cfg.arrival = &ack.Service{Choice: choice, Dir: t.TempDir(), Send: func(_ context.Context, in ack.Input, row ack.Row) error {
		if row.Name != "eyes" {
			t.Error("missing default")
		}
		reactions <- in.Message
		return nil
	}}
	aliases := newTestHandleAliasRegistry(t)
	for i := 0; i < 9; i++ {
		now := time.Now()
		raw, _ := json.Marshal(slackMessageEvent{Type: "message", User: "user-example", Channel: "DEXAMPLE", ChannelType: "im", TS: slackTimestamp(now.Add(time.Duration(i) * time.Microsecond)), Text: "synthetic"})
		processSlackEvent(cfg, aliases, nil, nil, nil, nil, slackEventEnvelope{Type: "event_callback", EventID: fmt.Sprintf("event-example-%d", i), Event: raw, receivedAt: now}, func() {})
	}
	if len(capture.snapshot()) != 9 {
		t.Fatal("held choices blocked delivery")
	}
	seen := map[string]bool{}
	for i := 0; i < 9; i++ {
		select {
		case id := <-reactions:
			if seen[id] {
				t.Fatal("duplicate reaction")
			}
			seen[id] = true
		case <-time.After(3 * time.Second):
			t.Fatal("missing reaction")
		}
	}
	cfg.arrival.Wait()
	if maximum.Load() != 4 {
		t.Fatalf("maximum choices=%d", maximum.Load())
	}
}

func TestArrivalConfigConflicts(t *testing.T) {
	for _, tc := range []struct {
		ack, busy, company string
		bad                bool
	}{{"eyes", "eyes", "", true}, {"jev", "heart", "", true}, {"eyes", "hourglass", "1", true}, {"eyes", "hourglass", "0", false}, {"", "hourglass", "1", false}, {"white_check_mark", "hourglass", "", true}, {"auto", "hourglass", "", true}, {"model", "hourglass", "", true}} {
		env := baseSlackEnv()
		env["ACK_REACTION"] = tc.ack
		env["BUSY_REACTION"] = tc.busy
		env["SLACK_COMPANY_VISIBLE_ACKS"] = tc.company
		_, err := loadConfigFromEnv(stubEnv(env))
		if (err != nil) != tc.bad {
			t.Fatalf("mode=%s busy=%s company=%s err=%v", tc.ack, tc.busy, tc.company, err)
		}
	}
}

func TestArrivalSlackTwinsAndSessionActs(t *testing.T) {
	choice, _ := ack.New(ack.Options{Mode: "heart", Catalog: ack.Catalog})
	dir := t.TempDir()
	watch := stopwatch.New(dir, "slack", "release-example", nil)
	calls := 0
	service := &ack.Service{Choice: choice, Dir: dir, Inbound: func(in ack.Input) {
		watch.Record(stopwatch.Event{Event: "inbound", Conversation: in.Conversation, Message: in.Message, Thread: in.Thread, MessageAt: in.Time, At: in.Seen})
	}}
	cfg := config{arrival: service}
	now := time.Now()
	msg := slackMessageEvent{Type: "message", Channel: "CEXAMPLE", User: "UEXAMPLE", TS: slackTimestamp(now), Text: "synthetic", ChannelType: "im"}
	send := func(_ context.Context, _ ack.Input, row ack.Row) error {
		if row.Name != "heart" {
			t.Error("wrong Slack spelling")
		}
		calls++
		return nil
	}
	observeSlackArrival(cfg, slackEventEnvelope{receivedAt: now}, msg, true, send)
	service.Wait()
	msg.Type = "app_mention"
	observeSlackArrival(cfg, slackEventEnvelope{receivedAt: now}, msg, true, send)
	service.Wait()
	if calls != 1 {
		t.Fatal("twin repeated acknowledgement")
	}
	j := &inboundJournal{dir: filepath.Join(dir, "journal"), watch: watch}
	j.recordSessionAct(ingestionSessionAct{Kind: "reaction", Channel: msg.Channel, MessageTS: msg.TS, Source: "session_react", Timestamp: now.Add(time.Second)})
	j.recordSessionAct(ingestionSessionAct{Kind: "reply", Channel: msg.Channel, MessageTS: slackTimestamp(now.Add(2 * time.Second)), ThreadTS: msg.TS, Source: "session_publish", Timestamp: now.Add(2 * time.Second)})
	files, _ := filepath.Glob(filepath.Join(dir, "stopwatch-*.jsonl"))
	b, _ := os.ReadFile(files[0])
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 3 {
		t.Fatalf("events=%d", len(lines))
	}
	for _, line := range lines {
		var e map[string]any
		if json.Unmarshal([]byte(line), &e) != nil {
			t.Fatal("bad JSON")
		}
		if _, ok := e["text"]; ok {
			t.Fatal("text leaked")
		}
	}
}

func slackTimestamp(t time.Time) string { return fmt.Sprintf("%d.%06d", t.Unix(), t.Nanosecond()/1000) }

func TestStopwatchFileSharesUseSlackTime(t *testing.T) {
	dir := t.TempDir()
	watch := stopwatch.New(dir, "slack", "release-example", nil)
	cfg := config{inboundJournal: &inboundJournal{watch: watch}}
	var response slackCompleteUploadResp
	if err := json.Unmarshal([]byte(`{"ok":true,"files":[{"id":"file-example","shares":{"private":{"conversation-example":[{"ts":"1700000002.000001","thread_ts":"1700000000.000001"}]}}}]}`), &response); err != nil {
		t.Fatal(err)
	}
	if !recordFileShares(cfg, "conversation-example", response.Files) {
		t.Fatal("share timestamp missing")
	}
	files, _ := filepath.Glob(filepath.Join(dir, "stopwatch-*.jsonl"))
	if len(files) != 1 {
		t.Fatal("no journal")
	}
	data, _ := os.ReadFile(files[0])
	var event stopwatch.Event
	if err := json.Unmarshal(data, &event); err != nil {
		t.Fatal(err)
	}
	if event.Event != "words" || event.Thread != "1700000000.000001" || event.At.Unix() != 1700000002 {
		t.Fatal("wrong share time or thread")
	}
}

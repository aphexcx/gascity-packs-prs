package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Regression coverage for gf-xid finding 3f (the gf-pu2 vs-main gate's Major
// at main.go:2744): the dedup-hit branch of handlePublish answered a retry from
// the replayed receipt and RETURNED before the delivery-success bookkeeping
// the fresh-post path runs — clearing the busy reaction and recording the
// thread in the own-thread registry. A keyed publish whose first readback was
// unavailable is cached posted-but-unconfirmed; the retry re-verifies it
// through replayPublishReceipt and proves delivery, and that proof is the
// FIRST time anything knows the message landed. Skipping the bookkeeping there
// left the busy emoji on the inbound and the thread unregistered, so a
// mention-only session's later unmentioned replies in that thread excluded the
// session that had posted into it.
//
// The fix runs the bookkeeping exactly once, on the replay that re-verified
// the receipt, with the cached message ts as the root of a top-level post. A
// replay of a receipt that was already delivered when cached runs nothing: the
// original delivery consumed the busy mark and recorded the thread.

// replayBookkeepingStub is a Slack stub whose read leg is down for the first
// readback window and up afterwards, which records reactions.remove and
// answers chat.postMessage with ts.
func replayBookkeepingStub(t *testing.T, ts string, reads *atomic.Int32, posts *atomic.Int32) (*httptest.Server, *reactionRecorder) {
	t.Helper()
	rr := &reactionRecorder{ch: make(chan recordedReaction, 16)}
	var captured slackPostMessageReq
	fake := newFakeSlackPublishHandler(t, &captured, ts, func() { posts.Add(1) })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/reactions.remove"), strings.HasSuffix(r.URL.Path, "/reactions.add"):
			var body slackReactionsAddReq
			_ = json.NewDecoder(r.Body).Decode(&body)
			op := "add"
			if strings.HasSuffix(r.URL.Path, "/reactions.remove") {
				op = "remove"
			}
			rr.record(recordedReaction{op: op, channel: body.Channel, name: body.Name, timestamp: body.Timestamp})
			_, _ = fmt.Fprint(w, `{"ok":true}`)
		case strings.HasPrefix(r.URL.Path, "/conversations."):
			// Every attempt of the first publish's readback window fails;
			// the read leg is back for the retry.
			if reads.Add(1) <= int32(slackReadbackTimings.attempts) {
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			fake(w, r)
		default:
			fake(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, rr
}

func TestHandlePublishReplayThatReVerifiesDeliveryRunsTheDeliveryBookkeeping(t *testing.T) {
	fastReadbackTimings(t)
	cases := []struct {
		name     string
		replyTo  string // "" = top-level post
		markTS   string // the inbound ts carrying the busy mark
		wantRoot string // the own-thread root the session must be recorded under
	}{
		{name: "threaded reply joins its thread", replyTo: "100.000001", markTS: "100.000001", wantRoot: "100.000001"},
		{name: "top-level post roots a thread under the cached ts", replyTo: "", markTS: "100.000010", wantRoot: "1700.001"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var reads, posts atomic.Int32
			srv, reactions := replayBookkeepingStub(t, "1700.001", &reads, &posts)
			withSlackAPIStub(t, srv)

			marks := newBusyReactionRegistry()
			done, _ := marks.markBoth("C1", "", tc.markTS)
			close(done)
			own, err := newOwnThreadRegistry(filepath.Join(t.TempDir(), "own_threads.json"))
			if err != nil {
				t.Fatalf("own-thread registry: %v", err)
			}
			cfg := config{slackBotToken: "xoxb-test", busyReaction: "hourglass", busyMarks: marks, ownThreads: own}
			handler := handlePublish(cfg, nil, nil, newPublishDedupCache(publishDedupTTL))
			publish := func() publishReceipt {
				t.Helper()
				req := publishRequest{
					SessionID:        "gc-1",
					Conversation:     conversationRef{ConversationID: "C1", Kind: "room"},
					Text:             "hello",
					ReplyToMessageID: tc.replyTo,
					IdempotencyKey:   "k-1",
				}
				raw, _ := json.Marshal(req)
				rec := httptest.NewRecorder()
				handler(rec, httptest.NewRequest(http.MethodPost, "/publish", strings.NewReader(string(raw))))
				var receipt publishReceipt
				if err := json.Unmarshal(rec.Body.Bytes(), &receipt); err != nil {
					t.Fatalf("decode receipt %q: %v", rec.Body.String(), err)
				}
				return receipt
			}

			// First publish: Slack took the message, the read leg was down.
			// Nothing is proven, so nothing is cleared or recorded.
			first := publish()
			if first.Delivered || first.MessageID != "1700.001" {
				t.Fatalf("first receipt = %+v, want posted but unconfirmed", first)
			}
			reactions.assertNoCall(t, 150*time.Millisecond)
			if _, known := own.posters("C1", tc.wantRoot); known {
				t.Fatalf("an unconfirmed publish must not record the thread")
			}

			// The retry re-verifies the cached receipt: this replay is the
			// delivery proof, and the bookkeeping runs on it.
			second := publish()
			if !second.Delivered || second.MessageID != "1700.001" {
				t.Fatalf("retry receipt = %+v, want the re-verified message delivered", second)
			}
			if got := posts.Load(); got != 1 {
				t.Fatalf("chat.postMessage called %d times, want 1", got)
			}
			got := reactions.await(t, 2*time.Second)
			if got.op != "remove" || got.channel != "C1" || got.timestamp != tc.markTS {
				t.Fatalf("reaction = %+v, want remove of the busy mark on (C1, %s)", got, tc.markTS)
			}
			if sessions, known := own.posters("C1", tc.wantRoot); !known || len(sessions) != 1 || sessions[0] != "gc-1" {
				t.Fatalf("own-thread posters(C1, %s) = (%v, %v), want [gc-1] recorded by the re-verifying replay", tc.wantRoot, sessions, known)
			}

			// A replay of the now-delivered receipt runs the bookkeeping
			// again for nothing: no second remove, the record unchanged.
			third := publish()
			if !third.Delivered {
				t.Fatalf("third receipt = %+v, want the delivered receipt replayed", third)
			}
			reactions.assertNoCall(t, 150*time.Millisecond)
			if sessions, _ := own.posters("C1", tc.wantRoot); len(sessions) != 1 {
				t.Fatalf("posters after a plain replay = %v, want the one record", sessions)
			}
		})
	}
}

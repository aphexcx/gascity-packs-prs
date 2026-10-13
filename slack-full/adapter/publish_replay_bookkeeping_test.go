package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
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

// Concurrent retries of one posted-but-unconfirmed key can all read the
// message back; exactly one of them is the delivery proof and runs the
// bookkeeping (the lane's codex round 1: the slower retry's busy-reaction
// clear would otherwise take a mark a NEWER inbound registered in between).
func TestReplayPublishReceiptReportsTheReVerificationExactlyOnce(t *testing.T) {
	fastReadbackTimings(t)
	var captured slackPostMessageReq
	captured.Text = "hello"
	server := httptest.NewServer(newFakeSlackPublishHandler(t, &captured, "1700.001", nil))
	t.Cleanup(server.Close)
	withSlackAPIStub(t, server)

	dedup := newPublishDedupCache(publishDedupTTL)
	dedup.Put("k-1", publishReceipt{MessageID: "1700.001", Metadata: map[string]string{receiptMetadataKeyReadback: slackReadbackUnavailable}})
	var reVerified atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			replay, ok := replayPublishReceipt("xoxb-test", dedup, "k-1", readbackTarget("hello"))
			if !ok || !replay.receipt.Delivered {
				t.Errorf("replay = %+v, %v; want the re-verified delivered receipt", replay.receipt, ok)
			}
			if replay.reVerified {
				reVerified.Add(1)
			}
		}()
	}
	wg.Wait()
	if got := reVerified.Load(); got != 1 {
		t.Fatalf("%d of 8 concurrent replays reported the re-verification, want exactly 1", got)
	}
	if cached, ok := dedup.Get("k-1"); !ok || !cached.Delivered {
		t.Fatalf("cached receipt after the concurrent re-verifies = %+v, %v; want delivered", cached, ok)
	}
}

// A slower retry whose own readback stayed down writes the unconfirmed
// receipt it read BEFORE a faster retry proved delivery; that write must not
// undo the delivered receipt, or the next retry would re-verify it and run
// the bookkeeping a second time (the lane's codex round 2).
func TestReplayPublishReceiptStaleUnconfirmedWriteCannotReopenTheTransition(t *testing.T) {
	fastReadbackTimings(t)
	var captured slackPostMessageReq
	captured.Text = "hello"
	server := httptest.NewServer(newFakeSlackPublishHandler(t, &captured, "1700.001", nil))
	t.Cleanup(server.Close)
	withSlackAPIStub(t, server)

	dedup := newPublishDedupCache(publishDedupTTL)
	unconfirmed := publishReceipt{MessageID: "1700.001", Metadata: map[string]string{receiptMetadataKeyReadback: slackReadbackUnavailable}}
	dedup.Put("k-1", unconfirmed)

	first, ok := replayPublishReceipt("xoxb-test", dedup, "k-1", readbackTarget("hello"))
	if !ok || !first.receipt.Delivered || !first.reVerified {
		t.Fatalf("first replay = %+v, %v; want the re-verification", first, ok)
	}
	// The slower retry's stale write: the unconfirmed copy it took before.
	dedup.Put("k-1", unconfirmed)
	if cached, ok := dedup.Get("k-1"); !ok || !cached.Delivered {
		t.Fatalf("cached receipt after a stale unconfirmed write = %+v, %v; want the delivered receipt kept", cached, ok)
	}
	third, ok := replayPublishReceipt("xoxb-test", dedup, "k-1", readbackTarget("hello"))
	if !ok || !third.receipt.Delivered || third.reVerified {
		t.Fatalf("third replay = %+v, %v; want the delivered receipt replayed with no second re-verification", third, ok)
	}
}

// The replay's busy-mark clear must not take a mark an inbound registered in
// the thread AFTER the reply was posted: that inbound is not what the reply
// answered. The published message's ts is the cutoff — Slack ts are ordered
// within a channel — on both paths (the lane's codex round 3).
func TestHandlePublishReplayDoesNotClearABusyMarkNewerThanTheReply(t *testing.T) {
	fastReadbackTimings(t)
	var reads, posts atomic.Int32
	srv, reactions := replayBookkeepingStub(t, "1700.001", &reads, &posts)
	withSlackAPIStub(t, srv)

	marks := newBusyReactionRegistry()
	done, _ := marks.markBoth("C1", "", "1600.000")
	close(done)
	cfg := config{slackBotToken: "xoxb-test", busyReaction: "hourglass", busyMarks: marks}
	handler := handlePublish(cfg, nil, nil, newPublishDedupCache(publishDedupTTL))
	publish := func() publishReceipt {
		t.Helper()
		raw, _ := json.Marshal(publishRequest{
			SessionID:        "gc-1",
			Conversation:     conversationRef{ConversationID: "C1", Kind: "room"},
			Text:             "hello",
			ReplyToMessageID: "1600.000",
			IdempotencyKey:   "k-1",
		})
		rec := httptest.NewRecorder()
		handler(rec, httptest.NewRequest(http.MethodPost, "/publish", strings.NewReader(string(raw))))
		var receipt publishReceipt
		if err := json.Unmarshal(rec.Body.Bytes(), &receipt); err != nil {
			t.Fatalf("decode receipt %q: %v", rec.Body.String(), err)
		}
		return receipt
	}

	if first := publish(); first.Delivered {
		t.Fatalf("first receipt = %+v, want posted but unconfirmed", first)
	}
	// A newer inbound in the same thread, after the reply was posted
	// (its ts is newer than the reply's), takes the thread's mark.
	newer, _ := marks.markBoth("C1", "1600.000", "1700.002")
	close(newer)
	if ts, ok := marks.pending("C1", "1600.000"); !ok || ts != "1700.002" {
		t.Fatalf("pending mark before the retry = (%s, %v), want the newer inbound's", ts, ok)
	}

	if second := publish(); !second.Delivered {
		t.Fatalf("retry receipt = %+v, want delivered", second)
	}
	if ts, ok := marks.pending("C1", "1600.000"); !ok || ts != "1700.002" {
		t.Fatalf("pending mark after the re-verifying replay = (%s, %v), want the newer inbound's mark kept", ts, ok)
	}
	deadline := time.After(300 * time.Millisecond)
	for {
		select {
		case rec := <-reactions.ch:
			if rec.op == "remove" && rec.timestamp == "1700.002" {
				t.Fatalf("the replay removed the newer inbound's busy reaction: %+v", rec)
			}
			continue
		case <-deadline:
		}
		break
	}
}

// The cutoff must not leave an answered mark restorable (the lane's codex
// round 5): when a newer inbound displaced the older mark before the reply's
// proof came, takeBefore skips the newer mark — but the older one, displaced
// into the newer inbound's hands, is answered by this reply, and a later
// failure of the newer inbound's forward must remove it, not restore it. The
// same goes for an answered stale ancestor riding on the newer entry. The
// newer mark itself stays restorable: a reply older than it answered nothing
// of it.
func TestBusyReactionTakeBeforeBlocksRestoringTheAnsweredMarks(t *testing.T) {
	marks := newBusyReactionRegistry()
	const thread = "1600.000"
	older, _ := marks.markBoth("C1", "", thread) // the inbound the reply answers
	close(older)
	// The newer inbound displaces the mark before the reply's proof comes;
	// the older mark now rides in its hands for a possible restore.
	newerDone, displaced := marks.markBoth("C1", thread, "1700.002")
	if len(displaced) != 1 || displaced[0].mark.messageTS != thread {
		t.Fatalf("the newer inbound displaced %+v; want the older mark in its hands", displaced)
	}
	// A failed earlier re-target's rollback parks an even older ancestor on
	// the thread's entry as stale ancestry.
	marks.restoreDisplaced("C1", []busyDisplaced{{threadKey: thread, mark: busyTaken{messageTS: "1500.000"}}})

	// The reply, posted at 1700.001, is proven now: the newer mark is left
	// pending and the answered ancestor is taken.
	taken := marks.takeBefore("C1", thread, "1700.001")
	if len(taken) != 1 || taken[0].messageTS != "1500.000" {
		t.Fatalf("takeBefore = %+v, want the answered ancestor 1500.000 taken and nothing else", taken)
	}
	if ts, ok := marks.pending("C1", thread); !ok || ts != "1700.002" {
		t.Fatalf("pending after takeBefore = (%s, %v), want the newer mark kept", ts, ok)
	}

	// The newer inbound's forward fails: its mark goes, and the answered
	// mark it displaced is blocked from restoration (returned for removal).
	blocked := marks.cancelBoth("C1", thread, "1700.002", newerDone, displaced)
	var blockedTS []string
	for _, b := range blocked {
		blockedTS = append(blockedTS, b.messageTS)
	}
	if len(blocked) != 1 || blocked[0].messageTS != thread {
		t.Fatalf("cancelBoth blocked %v, want the answered mark %s blocked from restoration", blockedTS, thread)
	}
	if ts, ok := marks.pending("C1", thread); ok {
		t.Fatalf("pending after the cancel = %s, want nothing restored", ts)
	}
}

func TestBusyReactionTakeBeforeLeavesTheNewerMarkRestorable(t *testing.T) {
	marks := newBusyReactionRegistry()
	const thread = "1600.000"
	older, _ := marks.markBoth("C1", "", thread)
	close(older)
	newerDone, _ := marks.markBoth("C1", thread, "1700.002")
	close(newerDone)
	if taken := marks.takeBefore("C1", thread, "1700.001"); len(taken) != 0 {
		t.Fatalf("takeBefore = %+v, want nothing taken", taken)
	}
	// A third inbound displaces the newer mark and then fails: the newer
	// mark, which no reply answered, is restored.
	thirdDone, displaced := marks.markBoth("C1", thread, "1800.003")
	if blocked := marks.cancelBoth("C1", thread, "1800.003", thirdDone, displaced); len(blocked) != 0 {
		t.Fatalf("cancelBoth blocked %+v, want the newer mark restored, not blocked", blocked)
	}
	if ts, ok := marks.pending("C1", thread); !ok || ts != "1700.002" {
		t.Fatalf("pending after the third inbound's cancel = (%s, %v), want the newer mark restored", ts, ok)
	}
}

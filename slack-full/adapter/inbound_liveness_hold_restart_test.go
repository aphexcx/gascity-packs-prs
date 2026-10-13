package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"testing"
	"time"
)

// Regression coverage for gf-c7n finding 3l (the Opus 5.5 delta read's MAJOR
// on PR 46 at e8b760cf, introduced by round 4's 3g): the hold on a failed
// replay (PendingTS, the watermark held below it) was saved to the state
// file, but the record of which messages had ALREADY ARRIVED — the origins —
// lived only in memory. After an adapter restart the startup probe re-read
// from the held watermark and replayed every live-delivered line above it,
// up to an hour back: gc drops the duplicate channel copy, but mention-only
// and @handle lines are injected into sessions again with no dedup key, and
// their hourglasses come back. The read's throwaway test replayed the
// founder line once at the tip and zero times at 14439699. The origins a
// held channel knows above its watermark now ride the state file beside the
// hold, so a restart replays only what was never delivered.

func replayedCount(envs []slackEventEnvelope, ts string) int {
	n := 0
	for _, env := range envs {
		var ev struct {
			TS string `json:"ts"`
		}
		_ = json.Unmarshal(env.Event, &ev)
		if ev.TS == ts {
			n++
		}
	}
	return n
}

// The read's reproduction, committed: a replay fails (gc down while the
// adapter scans), a founder line is delivered live above the hold, the
// adapter restarts within the hour. The founder line replays ZERO times;
// the message that was never delivered replays once.
func TestLivenessRestartUnderAHoldReplaysOnlyWhatWasNeverDelivered(t *testing.T) {
	base := time.Date(2026, 10, 13, 2, 0, 0, 0, time.UTC)
	clock := &fakeClock{t: base}
	cfg := livenessTestConfig(t)
	cfg.livenessStatePath = filepath.Join(t.TempDir(), "liveness.json")
	hist := newFakeHistory()
	l1 := newTestLiveness(t, cfg, hist, clock)
	failing := &deliveryRecorder{status: http.StatusInternalServerError}
	l1.deliver = failing.deliver

	missed := tsAt(base, 2*time.Minute)
	hist.byChan["C1"] = []slackHistoryMessage{histMsg(missed, "U1", "missed while down")}
	clock.advance(12 * time.Minute)
	l1.tick(context.Background()) // the replay fails: a hold
	if got := l1.watermarkOf("C1"); got != tsAt(base, 0) {
		t.Fatalf("watermark after the failed replay = %s, want held at %s", got, tsAt(base, 0))
	}
	live := tsAt(base, 13*time.Minute)
	clock.advance(time.Minute)
	if got := l1.noteInboundEnvelope(liveMessageEnvelope(t, "EvF", "C1", live, "UFOUNDER", "founder line, delivered live"), ""); got != inboundOriginLive {
		t.Fatalf("live = %v, want live", got)
	}
	l1.saveState() // the persist ticker, or a graceful shutdown

	// Restart: fresh origins in memory, the persisted hold.
	hist.byChan["C1"] = append(hist.byChan["C1"], histMsg(live, "UFOUNDER", "founder line, delivered live"))
	clock.advance(time.Minute)
	l2 := newTestLiveness(t, cfg, hist, clock)
	ok := &deliveryRecorder{status: http.StatusOK}
	l2.deliver = ok.deliver
	l2.probe(context.Background(), probeModeReconnect, "startup (persisted watermarks)")
	envs := ok.all()
	if n := replayedCount(envs, live); n != 0 {
		t.Fatalf("the live-delivered founder line was replayed %d time(s) after the restart, want 0 (envelopes=%d)", n, len(envs))
	}
	if n := replayedCount(envs, missed); n != 1 {
		t.Fatalf("the never-delivered message was replayed %d time(s) after the restart, want 1 (envelopes=%d)", n, len(envs))
	}
	if got := l2.originSource("C1", live); got != inboundOriginLive {
		t.Fatalf("restored origin of the founder line = %v, want live", got)
	}
	if got := l2.watermarkOf("C1"); got != live {
		t.Fatalf("watermark after the restart's replay = %s, want the held advance %s released", got, live)
	}
}

// A replay in flight — marked before delivery so a racing live copy drops —
// is not a delivered line: the state file never carries it, so a crash
// before the delivery concludes cannot restore it as delivered.
func TestLivenessStateNeverPersistsAnInFlightReplayAsDelivered(t *testing.T) {
	base := time.Date(2026, 10, 13, 2, 0, 0, 0, time.UTC)
	clock := &fakeClock{t: base}
	cfg := livenessTestConfig(t)
	cfg.livenessStatePath = filepath.Join(t.TempDir(), "liveness.json")
	hist := newFakeHistory()
	l := newTestLiveness(t, cfg, hist, clock)
	held := tsAt(base, 2*time.Minute)
	inFlight := tsAt(base, 3*time.Minute)
	delivered := tsAt(base, 4*time.Minute)
	l.mu.Lock()
	l.holdPendingLocked(missedMessage{channel: "C1", msg: histMsg(held, "U1", "held"), watermark: l.channels["C1"].WatermarkTS}, true, false)
	l.originsPutLocked(originKey("C1", inFlight), inboundOriginBackfilled, clock.now())
	l.inFlight[originKey("C1", inFlight)] = true
	l.originsPutLocked(originKey("C1", delivered), inboundOriginBackfilled, clock.now())
	l.stateDirty = true
	l.mu.Unlock()
	l.saveState()
	reloaded := newTestLiveness(t, cfg, hist, clock)
	if got := reloaded.originSource("C1", inFlight); got != inboundOriginUnknown {
		t.Fatalf("restored origin of the in-flight replay = %v, want unknown", got)
	}
	if got := reloaded.originSource("C1", delivered); got != inboundOriginBackfilled {
		t.Fatalf("restored origin of the delivered replay = %v, want backfilled", got)
	}
}

// The state file carries at most livenessOriginMax origins; the loader's
// size limit admits a file that full.
func TestLivenessStateWithEveryOriginHeldStillLoads(t *testing.T) {
	base := time.Date(2026, 10, 13, 2, 0, 0, 0, time.UTC)
	clock := &fakeClock{t: base}
	cfg := livenessTestConfig(t)
	cfg.livenessStatePath = filepath.Join(t.TempDir(), "liveness.json")
	hist := newFakeHistory()
	l := newTestLiveness(t, cfg, hist, clock)
	l.mu.Lock()
	l.holdPendingLocked(missedMessage{channel: "C1", msg: histMsg(tsAt(base, time.Second), "U1", "held"), watermark: l.channels["C1"].WatermarkTS}, true, false)
	for i := 0; i < livenessOriginMax; i++ {
		l.originsPutLocked(originKey("C1", tsAt(base, 2*time.Second+time.Duration(i)*time.Millisecond)), inboundOriginLive, clock.now())
	}
	l.stateDirty = true
	l.mu.Unlock()
	l.saveState()
	reloaded := newTestLiveness(t, cfg, hist, clock)
	reloaded.mu.Lock()
	n := len(reloaded.origins)
	pending := len(reloaded.channels["C1"].PendingTS)
	reloaded.mu.Unlock()
	if n != livenessOriginMax || pending != 1 {
		t.Fatalf("reloaded origins=%d pending=%d, want %d and 1: the state file must load whole", n, pending, livenessOriginMax)
	}
	_ = fmt.Sprintf
}

// The lane's codex round 7 (P2): a live line is noted before its forward
// runs, so a line still buffered or forwarding is not delivered yet. Its
// origin rides the state file only once its event's dedup claim concluded
// committed; a crash before that leaves it for the recovery to replay.
func TestLivenessStatePersistsALiveLineOnlyOnceItsClaimCommitted(t *testing.T) {
	base := time.Date(2026, 10, 13, 2, 0, 0, 0, time.UTC)
	clock := &fakeClock{t: base}
	cfg := livenessTestConfig(t)
	cfg.livenessStatePath = filepath.Join(t.TempDir(), "liveness.json")
	cfg.eventDedup = newEventDedupCache(time.Minute)
	hist := newFakeHistory()
	l := newTestLiveness(t, cfg, hist, clock)
	held := tsAt(base, 2*time.Minute)
	live := tsAt(base, 3*time.Minute)
	l.mu.Lock()
	l.holdPendingLocked(missedMessage{channel: "C1", msg: histMsg(held, "U1", "held"), watermark: l.channels["C1"].WatermarkTS}, true, false)
	l.mu.Unlock()
	// The handler's order: the note, then the claim, then the forward.
	if got := l.noteInboundEnvelope(liveMessageEnvelope(t, "EvF", "C1", live, "UFOUNDER", "founder line"), ""); got != inboundOriginLive {
		t.Fatalf("live = %v", got)
	}
	cfg.eventDedup.begin("EvF")
	l.saveState()
	reloaded := newTestLiveness(t, cfg, hist, clock)
	if got := reloaded.originSource("C1", live); got != inboundOriginUnknown {
		t.Fatalf("restored origin of a line whose forward has not concluded = %v, want unknown (the recovery must replay it)", got)
	}
	// The forward reaches gc: the claim commits, and the next periodic save
	// carries the line without anything else dirtying the state (the lane's
	// codex round 8: the commit itself does not).
	cfg.eventDedup.commit("EvF")
	l.saveState()
	reloaded = newTestLiveness(t, cfg, hist, clock)
	if got := reloaded.originSource("C1", live); got != inboundOriginLive {
		t.Fatalf("restored origin of a delivered line = %v, want live", got)
	}
	// Restored, the line stays delivered through a further save and
	// restart: its presence in the file was the proof.
	reloaded.mu.Lock()
	reloaded.stateDirty = true
	reloaded.mu.Unlock()
	reloaded.saveState()
	again := newTestLiveness(t, cfg, hist, clock)
	if got := again.originSource("C1", live); got != inboundOriginLive {
		t.Fatalf("origin of a delivered line after a second restart = %v, want live", got)
	}
}

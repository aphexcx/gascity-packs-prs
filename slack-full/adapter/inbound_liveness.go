package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"net/http"
	"net/url"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

// Inbound-liveness watchdog + history backfill (gp-3og / ci-mk4qj).
//
// WHY: on 2026-08-19 Slack stopped delivering Events API POSTs to the
// adapter for ~26 minutes. Nothing in the adapter failed — the
// listener was up, /healthz said ok, outbound publishes worked — so
// nothing alarmed, and a merge order posted in Slack was simply never
// seen until a human asked in a terminal. The gap is structural: an
// inbound transport that goes quiet looks exactly like a quiet
// workspace. The one thing that distinguishes the two is the channel
// history the bot token can read — if conversations.history shows new
// human messages the adapter never received, inbound is dead.
//
// What this file does:
//
//   - Tracks, on every verified event_callback from ANY transport, the
//     last-inbound clock, a per-channel watermark (newest message ts
//     seen) and a bounded origin seen-set (channel|ts → live/backfilled).
//     The learned channel set plus the operator-pinned
//     SLACK_LIVENESS_CHANNELS form the watched set.
//   - Watchdog: when no inbound has arrived for SLACK_LIVENESS_STALL_AFTER
//     (default 10m), probes each watched channel's history above its
//     watermark. New human-authored, dispatch-eligible messages that the
//     adapter never saw ⇒ ALARM: a loud log line, /healthz flips to
//     inbound_liveness=stalled, and — when SLACK_LIVENESS_ALERT_CHANNEL is
//     set — a chat.postMessage to that channel (outbound is independent
//     of the dead inbound path, which is the whole point). Recovery is
//     logged (and posted) when live inbound resumes.
//   - Backfill: the same probe machinery replays missed messages through
//     the normal event pipeline as synthesized event_callback envelopes
//     (event_id "backfill:<channel>:<ts>", trusted-transport request), so
//     they reach the bound sessions late rather than never. It runs on
//     Socket Mode (re)connect — covering the socket's own outages and
//     adapter restarts via the persisted watermarks — and on a watchdog
//     alarm. SLACK_BACKFILL_MAX_WINDOW=0 turns delivery off (alarm only).
//   - Dedup both ways: the probe skips any origin already seen live; the
//     live path (handleSlackEvents) drops a message whose origin the
//     backfill already delivered. The message/app_mention twin pair for
//     a bot mention is unaffected — neither is ever in the backfilled set
//     unless the backfill delivered that message.
//
// Limits, deliberately: conversations.history does not list thread
// replies, so the probe also scans recent parents whose latest_reply
// moved past the watermark and reads those threads (capped per probe).
// Channels the bot is not a member of return not_in_channel and are
// skipped — the same channels never produce events, so nothing is
// missed there either.

// Watchdog pacing.
const (
	// livenessTick is the watchdog's clock resolution.
	livenessTick = 30 * time.Second
	// livenessProbeMinInterval bounds how often a stalled adapter
	// re-reads channel history (per tick it probes at most once per
	// this interval, per channel set).
	livenessProbeMinInterval = 2 * time.Minute
	// livenessAlertRepeat re-posts the Slack alarm while a stall
	// persists, so a long outage is not a single easily-missed line.
	livenessAlertRepeat = 30 * time.Minute
	// livenessPersistInterval paces state-file writes while dirty.
	livenessPersistInterval = 60 * time.Second
	// livenessHoldRetryInterval bounds how often a held replay (a failed
	// replay waiting for gc) is retried while live traffic flows and no
	// stall probe would run: one retry probe per interval, no alarm, until
	// the hold lands or ages out of the backfill window (the Opus read's
	// minor beside gf-c7n finding 3l: a held founder line waited for the
	// next stall, reconnect or restart, and could age out unreplayed).
	livenessHoldRetryInterval = 2 * time.Minute
	// livenessHeldTextMax bounds the text a held replay keeps for its dead
	// letter (bytes, cut on a rune boundary); files are kept by count only.
	livenessHeldTextMax = 4096
	// livenessOriginTTL / livenessOriginMax bound the origin seen-set.
	livenessOriginTTL = 3 * time.Hour
	livenessOriginMax = 16384
	// livenessMaxChannels caps the watched set (most recently active win).
	livenessMaxChannels = 50
	// livenessHistoryLimit is the per-call conversations.history page.
	livenessHistoryLimit = 200
	// livenessMaxHistoryPages bounds the conversations.history pages one
	// probe reads above one channel's watermark (gf-d7b finding 3o: the
	// read used to take one page and drop has_more, so the newest 200
	// messages of a longer gap replayed and the watermark moved past the
	// rest). Pages a probe cannot finish are carried on the channel
	// (PendingHistory) and read first by the next probe. A var for tests.
	// livenessMaxThreadsPerChannel caps thread reads per channel per probe.
	livenessMaxThreadsPerChannel = 10
	// livenessThreadScanWindow is how far back parents are scanned for
	// fresh replies (history only returns parents by their own ts).
	livenessThreadScanWindow = 24 * time.Hour
	// livenessMaxPendingThreads caps the threads a channel remembers as
	// pending (a read that failed, or found more than one probe replayed):
	// each is re-read every probe from its own cursor until drained.
	livenessMaxPendingThreads = 32
	// livenessFetchTimeout bounds one Slack read (one page).
	livenessFetchTimeout = 15 * time.Second
	// livenessMaxBackfillPerChannel caps replayed messages per channel
	// per pass so a long outage cannot flood sessions in one burst.
	livenessMaxBackfillPerChannel = 100
)

var livenessMaxHistoryPages = 5

// inboundOriginSource classifies a known message origin.
type inboundOriginSource uint8

const (
	inboundOriginUnknown inboundOriginSource = iota
	inboundOriginLive
	inboundOriginBackfilled
)

// Trusted-transport labels the liveness hook distinguishes.
const (
	transportBackfill   = "backfill"
	transportSocketMode = "socket_mode"
)

// slackHistoryMessage is the subset of a conversations.history /
// conversations.replies message the probe inspects. Raw keeps the full
// object for synthesis.
type slackHistoryMessage struct {
	Type        string          `json:"type"`
	Subtype     string          `json:"subtype,omitempty"`
	User        string          `json:"user,omitempty"`
	BotID       string          `json:"bot_id,omitempty"`
	Text        string          `json:"text,omitempty"`
	TS          string          `json:"ts"`
	ThreadTS    string          `json:"thread_ts,omitempty"`
	ReplyCount  int             `json:"reply_count,omitempty"`
	LatestReply string          `json:"latest_reply,omitempty"`
	Files       []slackFile     `json:"files,omitempty"`
	Raw         json.RawMessage `json:"-"`
}

// historyFetcher is the Slack read surface the watchdog needs; the
// production implementation uses the bot token, tests fake it.
type historyFetcher interface {
	// history returns one page of channel: messages with oldest < ts (and
	// ts < latest when latest is set — the page below the last one read;
	// Slack pages newest-first), oldest-first, at most limit, and whether
	// Slack reports more past this page (has_more): a short page proves
	// nothing on its own.
	history(ctx context.Context, channel, oldest, latest string, limit int) ([]slackHistoryMessage, bool, error)
	// replies returns the thread rooted at threadTS with ts > oldest,
	// oldest-first (the parent is omitted), at most limit, and whether
	// Slack reports more past this page (has_more): a short page proves
	// nothing on its own — the parent is dropped from it, and Slack may
	// answer fewer than limit with more to come.
	replies(ctx context.Context, channel, threadTS, oldest string, limit int) ([]slackHistoryMessage, bool, error)
}

// livenessChannel is one watched channel's state.
type livenessChannel struct {
	TeamID      string    `json:"team_id,omitempty"`
	WatermarkTS string    `json:"watermark_ts"`
	LastSeenAt  time.Time `json:"last_seen_at"`
	Pinned      bool      `json:"pinned,omitempty"`
	// PendingTS holds the ts of replays that did not commit, and of missed
	// messages a pass never reached — the per-channel cap, a cancellation
	// (gf-d7b finding 3n) — (oldest first, capped at
	// livenessMaxBackfillPerChannel): the watermark never crosses
	// PendingTS[0] until that message lands — a committed retry or a live
	// copy — so the next probe re-reads it, and nothing delivered or seen
	// above it is lost: those origins are known and skipped (gf-xid finding
	// 3g, the lane's codex round 1). HeldMaxTS is the newest ts an advance
	// was refused for while held; the watermark jumps to it when the last
	// pending replay lands.
	PendingTS []string `json:"pending_ts,omitempty"`
	HeldMaxTS string   `json:"held_max_ts,omitempty"`
	// HeldOrigins is written by saveState and read by loadState only: the
	// origins the channel knows above its held watermark — delivered by a
	// replay that committed, or seen live — while a replay is pending, so a
	// restart replays only what was never delivered (gf-c7n finding 3l; the
	// origins are otherwise in memory only, and the startup probe re-read
	// everything above the hold). A replay still in flight is never one of
	// them.
	HeldOrigins []livenessHeldOrigin `json:"held_origins,omitempty"`
	// PendingMessages carries, by ts, the held replays themselves (the
	// history message, the attempts so far, when it was first held), so a
	// hold that ages out of the backfill window goes to the adapter's
	// inbound dead letter with its text, never silently.
	PendingMessages map[string]livenessHeldReplay `json:"pending_messages,omitempty"`
	// PendingThreads are the threads a probe could not finish reading
	// (gf-c7n finding 3i): a thread-history read that failed, or one whose
	// fresh replies outran what one probe read and replayed. Each is re-read
	// every probe from its own cursor (Oldest: below every reply not yet
	// delivered) whether or not the channel's watermark — which keeps moving
	// with the channel messages and the live traffic — still lists it, and
	// is dropped once a read found nothing undelivered past the cursor. The
	// watermark is never held for a thread read: the thread carries its own
	// progress. Capped at livenessMaxPendingThreads, oldest parent first;
	// a thread whose parent has left the thread scan window retires.
	PendingThreads []livenessPendingThread `json:"pending_threads,omitempty"`
	// PendingHistory holds the stretches of channel history a probe could
	// not finish reading (gf-d7b finding 3o): the pages below the ones it
	// read, above From (the watermark as that probe read it) and below
	// Below (the oldest ts it did read). Disjoint, lowest first; one is
	// installed the moment a probe leaves it (before any replay or live
	// message can move the watermark past it) and overlapping ones merge.
	// The next probe reads them first, page by page from Below down, and a
	// stretch shrinks or drops — once Slack reports nothing more — only
	// after the backfill delivered or held what was read from it; the
	// watermark moves on with what was read, and this is what keeps the
	// unread pages reachable below it. livenessMaxHistoryPages pages per
	// probe across the stretches; one the backfill window closes on
	// retires, loudly.
	PendingHistory []livenessPendingHistory `json:"pending_history,omitempty"`
}

// livenessPendingHistory is one stretch of channel history still unread.
type livenessPendingHistory struct {
	From  string `json:"from"`  // exclusive: the watermark as the probe read it
	Below string `json:"below"` // exclusive: the oldest ts the probe did read
	// FloorCrossed says the backfill window's floor has entered the
	// stretch while it stood unread: its oldest part was lost and said so
	// once (the lane's codex round 2); From rides the floor from then on.
	FloorCrossed bool `json:"floor_crossed,omitempty"`
	// addedFrom/addedBelow is the range added into the stretch since a
	// probe read it (the messages the backfill left inside it: the lane's
	// codex round 5): that probe's settle shrinks or drops what it read and
	// re-adds this range, so neither the unread remainder nor the messages
	// left is lost. In memory only: a restart re-reads the whole stretch.
	addedFrom, addedBelow string
}

// livenessPendingThread is one thread a channel still owes a read.
type livenessPendingThread struct {
	ParentTS string `json:"parent_ts"`
	Oldest   string `json:"oldest"` // replies above it are not all delivered yet
	// FloorCrossed says the backfill window's floor has passed the cursor
	// while the reads kept failing: the replies between them were
	// dead-lettered once (gf-d7b finding 3q), and the cursor rides the
	// floor from then on until the thread is read through or retires.
	FloorCrossed bool `json:"floor_crossed,omitempty"`
}

// livenessHeldReplay is one held replay: the message, its attempts, and
// when it was first held.
type livenessHeldReplay struct {
	Msg      slackHistoryMessage `json:"msg"` // text clipped to livenessHeldTextMax, files dropped
	TeamID   string              `json:"team_id,omitempty"`
	Attempts int                 `json:"attempts"`
	HeldAt   time.Time           `json:"held_at"`
	// FilesDropped counts the files the message carried; TextDropped says
	// the text was shed to fit the state file (the dead letter says so).
	FilesDropped int  `json:"files_dropped,omitempty"`
	TextDropped  bool `json:"text_dropped,omitempty"`
	// Listing says the hold stands for a thread's history listing, not a
	// message the adapter read (gf-d7b finding 3m): Msg carries the
	// thread's latest reply ts and its parent (ThreadTS), nothing else, and
	// the replies of that thread above Cursor (the probe's reading) are
	// what was never read. Released when the latest reply lands or a probe
	// reads the thread through (settlePendingThreads); the dead letter
	// names the thread and the cursor (gf-d7b finding 3p).
	Listing bool   `json:"listing,omitempty"`
	Cursor  string `json:"cursor,omitempty"`
	// FloorCrossed says the backfill window's floor has passed a listing
	// hold's cursor: the replies between were dead-lettered once (the
	// lane's codex round 4), and the cursor rides the floor from then on.
	FloorCrossed bool `json:"floor_crossed,omitempty"`
}

// clipHeldMessage returns the message as a held replay keeps it: the text
// cut to livenessHeldTextMax on a rune boundary, the files kept by count.
func clipHeldMessage(m slackHistoryMessage) (slackHistoryMessage, int) {
	files := len(m.Files)
	m.Files = nil
	m.Raw = nil
	if len(m.Text) > livenessHeldTextMax {
		cut := livenessHeldTextMax
		for cut > 0 && !utf8.RuneStart(m.Text[cut]) {
			cut--
		}
		m.Text = m.Text[:cut] + "…"
	}
	return m, files
}

// livenessRetiredHold is a held replay that aged out, for the dead letter —
// or (gf-d7b finding 3q) a pending thread whose undelivered replies the
// adapter can no longer read: threadTS names the parent, cursor the ts its
// replies were not delivered above, and reason says why (the backfill
// window's floor passed the cursor while its reads kept failing; the
// thread's parent left the thread scan window and it retired).
type livenessRetiredHold struct {
	channel string
	ts      string
	held    livenessHeldReplay
	known   bool // the message itself was recorded with the hold
	// a pending thread's loss, not a held replay's:
	threadTS string
	cursor   string
	reason   string
}

// livenessHeldOrigin is one persisted origin of a held channel.
type livenessHeldOrigin struct {
	TS   string `json:"ts"`
	Live bool   `json:"live,omitempty"`
}

// livenessState is the persisted shape.
type livenessState struct {
	LastInboundAt time.Time                   `json:"last_inbound_at"`
	Channels      map[string]*livenessChannel `json:"channels"`
}

type originEntry struct {
	source inboundOriginSource
	at     time.Time
	// eventID is the Slack event a live origin was noted from: its dedup
	// claim (begun after the note, concluded when the forward reached gc
	// or durable retry ownership) says whether the line was delivered.
	// delivered latches that conclusion once seen, so the claim's TTL sweep
	// cannot turn a delivered line back into an unknown one.
	eventID   string
	delivered bool
}

// missedMessage is one history message the adapter never received.
type missedMessage struct {
	channel string
	teamID  string
	msg     slackHistoryMessage
	// watermark is the channel's watermark when the probe read it, below
	// every missed ts: where the watermark goes back to if a live advance
	// crossed a replay that then did not commit.
	watermark string
}

// inboundLiveness is the process-wide tracker. Safe for concurrent use.
type inboundLiveness struct {
	cfg   config
	fetch historyFetcher
	// deliver runs one synthesized envelope through the events handler
	// and returns the HTTP status it would have produced. Wired by
	// main(); nil means "alarm only" (tests and disabled backfill).
	deliver func(ctx context.Context, env slackEventEnvelope) backfillDelivery
	// alert posts one alarm/recovery line to the operator channel; nil
	// disables. Wired only when SLACK_LIVENESS_ALERT_CHANNEL is set.
	alert func(ctx context.Context, text string) error
	now   func() time.Time

	mu            sync.Mutex
	startedAt     time.Time
	lastInboundAt time.Time
	lastProbeAt   time.Time
	lastAlertAt   time.Time
	stalledSince  time.Time
	alerted       bool
	channels      map[string]*livenessChannel
	origins       map[string]originEntry
	// inFlight marks the origins a replay has claimed (marked backfilled so
	// a racing live copy drops) but not yet concluded: not delivered, so
	// never persisted as such (gf-c7n finding 3l).
	inFlight   map[string]bool
	statePath  string
	stateDirty bool
	botUserID  string
	apiAppID   string
	teamID     string
	probing    bool

	probesTotal     atomic.Uint64
	missedTotal     atomic.Uint64
	backfilledTotal atomic.Uint64
	alarmsTotal     atomic.Uint64
	lastMissed      atomic.Int64
}

// livenessStateMaxBytes bounds the state file the loader accepts; saveState
// sheds the held replays' texts, then the held origins, until the file
// fits, so a state the writer produced is one the loader takes (the lane's
// codex round 7). A variable so tests can lower it.
var livenessStateMaxBytes = 8 << 20

// livenessHealth is the process-singleton tracker read by handleHealthz.
var livenessHealth atomic.Pointer[inboundLiveness]

// newInboundLiveness builds the tracker, loading persisted watermarks
// from cfg.livenessStatePath (missing/corrupt file ⇒ fresh state, logged)
// and seeding pinned channels.
func newInboundLiveness(cfg config, fetch historyFetcher) *inboundLiveness {
	l := &inboundLiveness{
		cfg:       cfg,
		fetch:     fetch,
		now:       time.Now,
		channels:  make(map[string]*livenessChannel),
		origins:   make(map[string]originEntry),
		inFlight:  make(map[string]bool),
		statePath: cfg.livenessStatePath,
		teamID:    cfg.accountID,
	}
	l.startedAt = l.now()
	l.loadState()
	for _, c := range cfg.livenessChannels {
		ch := l.channels[c]
		if ch == nil {
			ch = &livenessChannel{TeamID: cfg.accountID, LastSeenAt: l.startedAt}
			l.channels[c] = ch
		}
		ch.Pinned = true
		if ch.WatermarkTS == "" {
			// No history: watch from now. Anything older predates the
			// watchdog and is not "missed".
			ch.WatermarkTS = slackTSFromTime(l.startedAt)
		}
	}
	return l
}

// slackTSFromTime renders t as a Slack ts string ("<sec>.<micro>").
func slackTSFromTime(t time.Time) string {
	return fmt.Sprintf("%d.%06d", t.Unix(), t.Nanosecond()/1000)
}

// slackTSAfter and slackTSBefore return the ts one microsecond above and
// below ts: the exclusive bounds that include ts. A malformed ts is returned
// unchanged.
func slackTSAfter(ts string) string  { return slackTSShift(ts, 1) }
func slackTSBefore(ts string) string { return slackTSShift(ts, -1) }

func slackTSShift(ts string, by int64) string {
	sec, frac, ok := strings.Cut(ts, ".")
	if !ok || len(frac) != 6 {
		return ts
	}
	n, err := strconv.ParseInt(sec+frac, 10, 64)
	if err != nil || n+by < 0 {
		return ts
	}
	n += by
	return fmt.Sprintf("%d.%06d", n/1000000, n%1000000)
}

// Slack ts ordering reuses compareSlackTS (company_replay.go): numeric on
// (seconds, fraction), malformed values sort last.

// originKey is the seen-set key for a message.
func originKey(channel, ts string) string { return channel + "|" + ts }

// noteInboundEnvelope records one verified event_callback. transport is
// the trusted-transport label ("" for the Events API, "socket_mode",
// "backfill"). Returns inboundOriginBackfilled when the envelope is a
// live copy of a message the backfill already delivered — the caller
// drops it. Nil-safe (returns unknown, records nothing).
func (l *inboundLiveness) noteInboundEnvelope(env slackEventEnvelope, transport string) inboundOriginSource {
	if l == nil || env.Type != "event_callback" {
		return inboundOriginUnknown
	}
	if transport == transportBackfill {
		// Our own replay: bookkeeping already happened in backfill().
		return inboundOriginUnknown
	}
	var ev struct {
		Type    string `json:"type"`
		Channel string `json:"channel"`
		TS      string `json:"ts"`
	}
	_ = json.Unmarshal(env.Event, &ev)
	now := l.now()

	l.mu.Lock()
	defer l.mu.Unlock()
	l.lastInboundAt = now
	l.stateDirty = true
	if id := env.botUserID(); id != "" {
		l.botUserID = id
	}
	if env.APIAppID != "" {
		l.apiAppID = env.APIAppID
	}
	if env.TeamID != "" && l.teamID == "" {
		l.teamID = env.TeamID
	}
	if !l.stalledSince.IsZero() {
		since := l.stalledSince
		l.stalledSince = time.Time{}
		alerted := l.alerted
		l.alerted = false
		log.Printf("inbound liveness: RECOVERED — live inbound resumed after %s (transport=%s)", now.Sub(since).Round(time.Second), transportLabel(transport))
		if alerted && l.alert != nil {
			go l.postAlert(fmt.Sprintf(":white_check_mark: gc-slack-adapter inbound liveness RECOVERED — live inbound resumed after %s (transport=%s).",
				now.Sub(since).Round(time.Second), transportLabel(transport)))
		}
	}
	if (ev.Type != "message" && ev.Type != "app_mention") || ev.Channel == "" || ev.TS == "" {
		return inboundOriginLive
	}
	key := originKey(ev.Channel, ev.TS)
	if e, ok := l.origins[key]; ok && e.source == inboundOriginBackfilled {
		return inboundOriginBackfilled
	}
	l.originsPutLocked(key, inboundOriginLive, now)
	if e, ok := l.origins[key]; ok {
		e.eventID = env.EventID
		l.origins[key] = e
	}
	l.touchChannelLocked(ev.Channel, env.TeamID, ev.TS, now)
	return inboundOriginLive
}

func transportLabel(t string) string {
	if t == "" {
		return "events_api"
	}
	return t
}

// touchChannelLocked advances channel's watermark to ts (if newer) and
// marks it recently active. Caller holds l.mu.
func (l *inboundLiveness) touchChannelLocked(channel, teamID, ts string, now time.Time) {
	ch := l.channels[channel]
	if ch == nil {
		ch = &livenessChannel{}
		l.channels[channel] = ch
		l.evictChannelsLocked()
	}
	if teamID != "" {
		ch.TeamID = teamID
	}
	ch.LastSeenAt = now
	l.stateDirty = true
	// A message that landed — delivered by a retry or seen live — releases
	// its pending hold. While any older replay is still pending the
	// watermark does not cross it: the advance is remembered (HeldMaxTS)
	// and taken when the last pending replay lands.
	// A listing hold (finding 3m) stands for a thread, not this message:
	// the latest reply landing does not say the older unread replies did,
	// so it stays until a probe reads the thread through
	// (settlePendingThreads; the lane's codex round 4).
	if i := slices.Index(ch.PendingTS, ts); i >= 0 && !ch.PendingMessages[ts].Listing {
		ch.PendingTS = slices.Delete(ch.PendingTS, i, i+1)
		delete(ch.PendingMessages, ts)
	}
	if len(ch.PendingTS) > 0 && compareSlackTS(ts, ch.PendingTS[0]) >= 0 {
		if ch.HeldMaxTS == "" || compareSlackTS(ts, ch.HeldMaxTS) > 0 {
			ch.HeldMaxTS = ts
		}
		return
	}
	if !ch.heldLocked() && ch.HeldMaxTS != "" {
		if compareSlackTS(ch.HeldMaxTS, ts) > 0 {
			ts = ch.HeldMaxTS
		}
		ch.HeldMaxTS = ""
	}
	if ch.WatermarkTS == "" || compareSlackTS(ts, ch.WatermarkTS) > 0 {
		ch.WatermarkTS = ts
	}
}

// advanceWatermarkLocked moves channel's watermark up to ts — the newest
// message a probe read above it — without landing anything: a hold keeps
// the watermark and remembers the advance (HeldMaxTS), exactly as a live
// advance is kept, and takes it when the last hold releases. Caller holds
// l.mu.
func (l *inboundLiveness) advanceWatermarkLocked(channel, ts string) {
	ch := l.channels[channel]
	if ch == nil || ts == "" {
		return
	}
	if len(ch.PendingTS) > 0 && compareSlackTS(ts, ch.PendingTS[0]) >= 0 {
		if ch.HeldMaxTS == "" || compareSlackTS(ts, ch.HeldMaxTS) > 0 {
			ch.HeldMaxTS = ts
			l.stateDirty = true
		}
		return
	}
	if ch.WatermarkTS == "" || compareSlackTS(ts, ch.WatermarkTS) > 0 {
		ch.WatermarkTS = ts
		l.stateDirty = true
	}
}

// heldLocked reports whether a pending replay keeps the channel's
// watermark from advancing. Caller holds l.mu.
func (ch *livenessChannel) heldLocked() bool {
	return len(ch.PendingTS) > 0
}

// originFloorLocked reports whether the channel's delivered record must
// ride the state file (a pending replay or a pending thread: the next
// probe reads below the watermark) and the ts above which it is needed —
// the lowest of the watermark and every pending thread's cursor. Caller
// holds l.mu.
func (ch *livenessChannel) originFloorLocked() (string, bool) {
	if len(ch.PendingTS) == 0 && len(ch.PendingThreads) == 0 && len(ch.PendingHistory) == 0 {
		return "", false
	}
	floor := ch.WatermarkTS
	lower := func(ts string) {
		if ts != "" && (floor == "" || compareSlackTS(ts, floor) < 0) {
			floor = ts
		}
	}
	for _, p := range ch.PendingThreads {
		lower(p.Oldest)
	}
	for _, ph := range ch.PendingHistory {
		lower(ph.From)
	}
	// A listing hold's thread reads from the hold's cursor (the lane's
	// codex rounds 2 and 3): the delivered record rides the file down to it.
	for _, held := range ch.PendingMessages {
		if held.Listing {
			lower(held.Cursor)
		}
	}
	return floor, true
}

// addStretchLocked records (from, below) as one more unread stretch of
// channel, installed AT ONCE — never deferred: a stretch must exist before
// any replay or live message moves the watermark past it, or a state save
// in between and a crash would restore the moved watermark with no stretch
// (the lane's codex round 3); only shrinking or dropping one waits for the
// backfill. A stretch it overlaps or touches merges with it (the union; a
// crossed flag carries over). Caller holds l.mu with the channel present.
func (l *inboundLiveness) addStretchLocked(channel, from, below string, work bool) {
	ch := l.channels[channel]
	st := livenessPendingHistory{From: from, Below: below}
	var kept []livenessPendingHistory
	for _, cur := range ch.PendingHistory {
		if compareSlackTS(cur.From, st.Below) <= 0 && compareSlackTS(st.From, cur.Below) <= 0 {
			if compareSlackTS(cur.From, st.From) < 0 {
				st.From = cur.From
			}
			if compareSlackTS(cur.Below, st.Below) > 0 {
				st.Below = cur.Below
			}
			st.FloorCrossed = st.FloorCrossed || cur.FloorCrossed
			// What was added into cur since its read: cur's own added
			// range and this one — when this one is WORK (messages a pass
			// left), not a registration of an unread range the read
			// above the watermark left again, which the settle subtracts
			// its read from like any other (the lane's codex round 7: the
			// same range re-registered every probe undid the progress of
			// every read of it).
			st.addedFrom, st.addedBelow = cur.addedFrom, cur.addedBelow
			if work {
				if st.addedFrom == "" || compareSlackTS(from, st.addedFrom) < 0 {
					st.addedFrom = from
				}
				if st.addedBelow == "" || compareSlackTS(below, st.addedBelow) > 0 {
					st.addedBelow = below
				}
			}
			continue
		}
		kept = append(kept, cur)
	}
	kept = append(kept, st)
	sort.Slice(kept, func(i, j int) bool { return compareSlackTS(kept[i].From, kept[j].From) < 0 })
	ch.PendingHistory = kept
	l.stateDirty = true
}

// settleStretchLocked takes what a probe read — (from, below) down to readTo,
// everything when done — out of the stretch that holds that range now (the
// one read, or the one it merged into since) and keeps the rest: the part
// under what was read, the part above it, and the range the backfill added
// into it meanwhile (addedFrom/addedBelow: the messages a pass left), as
// stretches of their own, the crossed flag carried by the lower remainder.
// A range no stretch holds any more (retired) is left alone. Applied after
// the backfill. Caller holds l.mu with the channel present.
func (l *inboundLiveness) settleStretchLocked(channel, from, below, readTo string, done bool) {
	ch := l.channels[channel]
	for i, cur := range ch.PendingHistory {
		if compareSlackTS(cur.From, from) > 0 || compareSlackTS(cur.Below, below) < 0 {
			continue
		}
		var pieces []livenessPendingHistory
		lowerTo := readTo // exclusive: readTo itself was read
		if done {
			lowerTo = slackTSAfter(from) // from itself was outside the read
		}
		if compareSlackTS(cur.From, lowerTo) < 0 && (!done || compareSlackTS(cur.From, from) < 0) {
			pieces = append(pieces, livenessPendingHistory{From: cur.From, Below: lowerTo, FloorCrossed: cur.FloorCrossed})
		}
		if compareSlackTS(below, cur.Below) < 0 {
			pieces = append(pieces, livenessPendingHistory{From: slackTSBefore(below), Below: cur.Below})
		}
		addedFrom, addedBelow := cur.addedFrom, cur.addedBelow
		ch.PendingHistory = slices.Delete(ch.PendingHistory, i, i+1)
		ch.PendingHistory = slices.Insert(ch.PendingHistory, i, pieces...)
		l.stateDirty = true
		switch {
		case done && len(pieces) == 0:
			log.Printf("inbound liveness: channel=%s read the unread history pages (%s, %s) through; no longer pending", channel, from, below)
		case done:
			log.Printf("inbound liveness: channel=%s read the unread history pages (%s, %s) through; %d stretch(es) around them still pending", channel, from, below, len(pieces))
		default:
			log.Printf("inbound liveness: channel=%s read the unread history pages down to %s; (%s, %s) still pending", channel, readTo, cur.From, readTo)
		}
		if addedFrom != "" {
			// The messages the replay left inside what was read stay
			// pending as their own stretch.
			l.addStretchLocked(channel, addedFrom, addedBelow, true)
			log.Printf("inbound liveness: channel=%s the messages the replay left in (%s, %s) stay pending", channel, addedFrom, addedBelow)
		}
		return
	}
}

// releaseHeldAdvanceLocked takes the advance the holds refused once the
// last of them is gone. Caller holds l.mu.
func (ch *livenessChannel) releaseHeldAdvanceLocked() {
	if ch.heldLocked() || ch.HeldMaxTS == "" {
		return
	}
	if compareSlackTS(ch.HeldMaxTS, ch.WatermarkTS) > 0 {
		ch.WatermarkTS = ch.HeldMaxTS
	}
	ch.HeldMaxTS = ""
}

// notePendingThreadLocked remembers that channel still owes thread a read:
// the replies above oldest are not all delivered. An existing entry keeps
// its own cursor. Reports whether the thread is pending afterwards: false
// when the pending set is full and could not take it, in which case the
// caller must keep the thread reachable some other way (the watermark must
// not pass its latest reply: holdThreadListingLocked) — a thread left to
// its latest_reply listing alone is lost once the watermark moves past that
// reply (gf-d7b finding 3m). Caller holds l.mu.
func (l *inboundLiveness) notePendingThreadLocked(channel, teamID, threadTS, oldest string) bool {
	ch := l.channels[channel]
	if ch == nil {
		ch = &livenessChannel{TeamID: teamID}
		l.channels[channel] = ch
		l.evictChannelsLocked()
	}
	for _, p := range ch.PendingThreads {
		if p.ParentTS == threadTS {
			return true
		}
	}
	if len(ch.PendingThreads) >= livenessMaxPendingThreads {
		log.Printf("inbound liveness: channel=%s already has %d pending thread(s); thread %s cannot be pending", channel, len(ch.PendingThreads), threadTS)
		return false
	}
	ch.PendingThreads = append(ch.PendingThreads, livenessPendingThread{ParentTS: threadTS, Oldest: oldest})
	sort.Slice(ch.PendingThreads, func(i, j int) bool {
		return compareSlackTS(ch.PendingThreads[i].ParentTS, ch.PendingThreads[j].ParentTS) < 0
	})
	l.stateDirty = true
	return true
}

// admitListingHoldLocked puts a listing hold into a full hold set whose
// newer entries are all listing holds: the newest MESSAGE hold anywhere in
// the set gives way (its message has its stretch), since a listing hold is
// a thread's only owner (the lane's codex rounds 10 and 11: a refused
// listing hold left the thread with no owner at all; the first fix looked
// for a victim above the hold's place only). A set with no message hold
// takes nothing more. Caller holds l.mu.
func (l *inboundLiveness) admitListingHoldLocked(ch *livenessChannel, ts string) {
	for j := len(ch.PendingTS) - 1; j >= 0; j-- {
		if !ch.PendingMessages[ch.PendingTS[j]].Listing {
			delete(ch.PendingMessages, ch.PendingTS[j])
			rest := slices.Delete(ch.PendingTS, j, j+1)
			i, _ := slices.BinarySearchFunc(rest, ts, compareSlackTS)
			ch.PendingTS = slices.Insert(rest, i, ts)
			return
		}
	}
}

// holdThreadListingLocked keeps a thread the pending set could not take
// reachable when a probe could not read it through either — its read
// failed, or its replies outran the page or the replay (gf-d7b finding 3m):
// the channel's watermark is held below the thread's latest reply (the
// pending-replay hold's shape: PendingTS, the pull-back to the probe's
// reading, the retry timer), so the history listing — which names a thread
// by a latest_reply above the watermark — names it again every probe, and
// the probe reads it (the cap is soft for a thread the pending set cannot
// take). The hold stands for the listing, not a message (Listing): it is
// released when that latest reply lands, when a probe reads the thread
// through, or when the pending set takes the thread (settlePendingThreads);
// one the window closes on goes to the dead letter naming the thread. A
// thread whose latest reply is unknown (a pending entry re-read) is pending
// already and never reaches here. Caller holds l.mu.
func (l *inboundLiveness) holdThreadListingLocked(channel, teamID, threadTS, latestReply, watermark string, advance bool) {
	if latestReply == "" {
		log.Printf("inbound liveness: channel=%s thread %s cannot be pending and its latest reply is unknown — left to its latest_reply listing", channel, threadTS)
		return
	}
	ch := l.channels[channel]
	// One listing hold per thread: an earlier one (an older latest reply)
	// gives way, its cursor carried — a lower one kept, or the read that
	// just delivered through moving past it (the lane's codex round 6:
	// two holds on one thread, and the lowest cursor won every probe).
	carried := ""
	for ts, held := range ch.PendingMessages {
		if !held.Listing || held.Msg.ThreadTS != threadTS || ts == latestReply {
			continue
		}
		carried = held.Cursor
		delete(ch.PendingMessages, ts)
		if i := slices.Index(ch.PendingTS, ts); i >= 0 {
			ch.PendingTS = slices.Delete(ch.PendingTS, i, i+1)
		}
	}
	m := missedMessage{channel: channel, teamID: teamID, msg: slackHistoryMessage{TS: latestReply, ThreadTS: threadTS}, watermark: watermark}
	if !l.holdPendingLocked(m, false, true) {
		// Every hold is a listing hold: a hundred threads held by their
		// listing on one channel. Said loudly; the thread is left to its
		// listing, which the hold set cannot keep.
		log.Printf("inbound liveness: channel=%s thread %s cannot be held either — the hold set is full of threads held by their listing (%d); its replies above %s are left to its latest_reply listing", channel, threadTS, len(ch.PendingTS), watermark)
		return
	}
	if held, ok := ch.PendingMessages[latestReply]; ok {
		if held.Cursor == "" && carried != "" {
			held.Cursor = carried
		}
		held.Listing = true
		// A failed read keeps the lowest cursor; a read that delivered a
		// page and found more moves it up to what was delivered, or the
		// next probe would read the same page again (the lane's codex
		// round 5).
		if held.Cursor == "" || compareSlackTS(watermark, held.Cursor) < 0 || (advance && compareSlackTS(watermark, held.Cursor) > 0) {
			held.Cursor = watermark
		}
		ch.PendingMessages[latestReply] = held
	}
	log.Printf("inbound liveness: channel=%s thread %s cannot be pending (%d pending already) — the watermark holds below its latest reply %s so the listing names it again", channel, threadTS, len(ch.PendingThreads), latestReply)
}

// releaseThreadListingHoldLocked drops the listing hold a thread no longer
// needs: it was read through, or the pending set took it. Caller holds l.mu.
func (l *inboundLiveness) releaseThreadListingHoldLocked(ch *livenessChannel, threadTS string) {
	for ts, held := range ch.PendingMessages {
		if !held.Listing || held.Msg.ThreadTS != threadTS {
			continue
		}
		delete(ch.PendingMessages, ts)
		if i := slices.Index(ch.PendingTS, ts); i >= 0 {
			ch.PendingTS = slices.Delete(ch.PendingTS, i, i+1)
		}
		l.stateDirty = true
	}
	ch.releaseHeldAdvanceLocked()
}

// threadRead is what one probe learned about one thread: where it read
// from, every reply it fetched above that (oldest first; the missed ones
// are in the probe's missed set, the rest were known or not dispatchable),
// whether Slack reported nothing more past the page, and the latest reply
// the listing named (empty for a pending entry re-read from its cursor).
type threadRead struct {
	channel     string
	threadTS    string
	oldest      string
	fetched     []string
	complete    bool
	latestReply string
}

// settlePendingThreads runs after the backfill with this probe's thread
// reads: each thread's cursor moves past the replies it fetched, oldest
// first — one that was known or not dispatchable, or one this pass
// delivered (or saw live) — up to the first undelivered one, and a pending
// thread is dropped once Slack reported nothing more past a page that left
// nothing undelivered and the channel's replay left nothing behind. A
// thread that is not pending becomes pending when Slack has more or the
// channel's replay was incomplete: a live advance may carry the watermark
// past what is left, and the thread's own cursor is what gets it read next
// probe.
func (l *inboundLiveness) settlePendingThreads(reads []threadRead, missed []missedMessage, incomplete map[string]bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	for _, r := range reads {
		wasMissed := map[string]bool{}
		for _, m := range missed {
			if m.channel == r.channel && m.msg.ThreadTS == r.threadTS {
				wasMissed[m.msg.TS] = true
			}
		}
		cursor := r.oldest
		drained := true
		for _, ts := range r.fetched { // oldest first
			if wasMissed[ts] {
				e, ok := l.origins[originKey(r.channel, ts)]
				if !ok || e.source == inboundOriginUnknown || now.Sub(e.at) > livenessOriginTTL {
					drained = false
					break
				}
			}
			cursor = ts
		}
		ch := l.channels[r.channel]
		if ch == nil {
			continue
		}
		done := drained && r.complete && !incomplete[r.channel]
		idx := -1
		for i, p := range ch.PendingThreads {
			if p.ParentTS == r.threadTS {
				idx = i
				break
			}
		}
		carried := true // the thread is read through, or pending with its cursor
		switch {
		case done && idx >= 0:
			ch.PendingThreads = slices.Delete(ch.PendingThreads, idx, idx+1)
			l.stateDirty = true
			log.Printf("inbound liveness: channel=%s thread=%s drained; no longer pending", r.channel, r.threadTS)
		case done:
		case idx >= 0:
			if compareSlackTS(cursor, ch.PendingThreads[idx].Oldest) > 0 {
				ch.PendingThreads[idx].Oldest = cursor
				l.stateDirty = true
			}
		default:
			carried = l.notePendingThreadLocked(r.channel, ch.TeamID, r.threadTS, cursor)
		}
		// A thread the pending set holds, or one read through, needs no
		// listing hold; one with undelivered replies the pending set could
		// not take keeps (or gets) one, so the watermark cannot pass its
		// latest reply (the pull-back goes to this probe's reading, below
		// every reply it fetched). A thread read through whose channel's
		// replay was cut elsewhere needs neither: what was cut is held.
		switch {
		case carried:
			l.releaseThreadListingHoldLocked(ch, r.threadTS)
		case !(drained && r.complete):
			l.holdThreadListingLocked(r.channel, ch.TeamID, r.threadTS, r.latestReply, cursor, true)
		}
	}
}

// retireExpiredPendingLocked drops the pending holds whose message has aged
// out of the backfill window: the probe reads above the window's floor, so
// such a message can never be re-read and its hold could never be released
// by a retry — the watermark would stay frozen for good, and with origins
// unpersisted every restart would replay the whole window (the lane's codex
// round 2). Retired messages are returned for dead-letter recording; the
// last hold to go releases the held advance. With backfill disabled (no window) nothing can release a
// hold, so every one is retired. Caller holds l.mu.
func (l *inboundLiveness) retireExpiredPendingLocked(now time.Time) []livenessRetiredHold {
	var out []livenessRetiredHold
	floor := ""
	if win := l.cfg.backfillMaxWindow; win > 0 {
		floor = slackTSFromTime(now.Add(-win))
	}
	for id, ch := range l.channels {
		// A pending thread whose parent has left the thread scan window is
		// one the adapter would never list again anyway: retirement records
		// its unread reply range for the dead letter. With backfill
		// disabled nothing replays, so every one is retired.
		if len(ch.PendingThreads) > 0 {
			scanFloor := slackTSFromTime(now.Add(-livenessThreadScanWindow))
			keptThreads := ch.PendingThreads[:0]
			var retiredThreads []string
			for _, p := range ch.PendingThreads {
				if floor == "" || compareSlackTS(p.ParentTS, scanFloor) < 0 {
					// Retired: the adapter would never list the parent
					// again. Its replies above the cursor were never
					// delivered, and Slack never redelivers a missed event:
					// the thread goes to the inbound dead letter (gf-d7b
					// finding 3q), logged and posted.
					retiredThreads = append(retiredThreads, p.ParentTS)
					why := fmt.Sprintf("inbound liveness: pending thread retired — its parent left the thread scan window (%s) with its replies above %s never delivered to gc (the reads kept failing); Slack does not redeliver a missed event: read the thread from Slack and re-post the replies by hand", livenessThreadScanWindow, p.Oldest)
					if floor == "" {
						why = fmt.Sprintf("inbound liveness: pending thread retired — the backfill is disabled, so its replies above %s are never delivered to gc; read the thread from Slack and re-post them by hand", p.Oldest)
					}
					out = append(out, livenessRetiredHold{channel: id, threadTS: p.ParentTS, cursor: p.Oldest, reason: why})
					continue
				}
				if !p.FloorCrossed && compareSlackTS(p.Oldest, floor) < 0 {
					// The window's floor has passed the cursor while the
					// reads kept failing: the replies between them can no
					// longer be read (the probe reads above the floor).
					// Dead-lettered once, with the cursor; the cursor rides
					// the floor from here (probeChannel's clamp) until the
					// thread is read through or retires.
					p.FloorCrossed = true
					out = append(out, livenessRetiredHold{channel: id, threadTS: p.ParentTS, cursor: p.Oldest, reason: fmt.Sprintf("inbound liveness: pending thread's replies above %s fell below the backfill window's floor (%s) while its reads kept failing — never delivered to gc, and no longer read: read the thread from Slack and re-post them by hand; the thread stays pending above the floor", p.Oldest, l.cfg.backfillMaxWindow)})
					l.stateDirty = true
				}
				keptThreads = append(keptThreads, p)
			}
			if len(retiredThreads) > 0 {
				clear(ch.PendingThreads[len(keptThreads):])
				ch.PendingThreads = keptThreads
				l.stateDirty = true
				log.Printf("inbound liveness: channel=%s retired %d pending thread(s) out of the thread scan window (%s) — their undelivered replies were NEVER delivered to gc and Slack does not redeliver them: going to the inbound dead letter",
					id, len(retiredThreads), strings.Join(retiredThreads, " "))
			}
		}
		// Unread history pages the backfill window has closed on can never
		// be read (the probe reads above the window's floor); what is left
		// above the floor stays pending.
		if len(ch.PendingHistory) > 0 {
			var kept []livenessPendingHistory
			for _, ph := range ch.PendingHistory {
				switch {
				case floor == "" || compareSlackTS(ph.Below, floor) <= 0:
					l.stateDirty = true
					log.Printf("inbound liveness: channel=%s retired the unread history pages (%s, %s) out of the backfill window — NEVER READ: whatever human messages they held were never delivered to gc; read the channel's history for that stretch by hand", id, ph.From, ph.Below)
					if l.alert != nil {
						go l.postAlert(fmt.Sprintf(":warning: gc-slack-adapter: a stretch of <#%s> history (ts %s to %s) was never read within the backfill window (%s) and is lost to the backfill. Read it by hand.", id, ph.From, ph.Below, l.cfg.backfillMaxWindow))
					}
					continue
				case compareSlackTS(ph.From, floor) < 0:
					if !ph.FloorCrossed {
						// Said once: the stretch's oldest part is being lost
						// as the floor rises while the reads keep failing; the
						// full expiry below says the rest.
						ph.FloorCrossed = true
						log.Printf("inbound liveness: channel=%s the backfill window's floor (%s) has entered the unread history pages (%s, %s) — NEVER READ below the floor: whatever human messages they held are lost to the backfill as the floor rises; read the channel's history for that stretch by hand", id, floor, ph.From, ph.Below)
						if l.alert != nil {
							go l.postAlert(fmt.Sprintf(":warning: gc-slack-adapter: a stretch of <#%s> history (ts %s to %s) is still unread as the backfill window (%s) closes on it; what is below the floor is lost to the backfill. Read it by hand.", id, ph.From, ph.Below, l.cfg.backfillMaxWindow))
						}
					}
					ph.From = floor
					l.stateDirty = true
				}
				kept = append(kept, ph)
			}
			ch.PendingHistory = kept
		}
		// A listing hold's cursor the floor passes: the replies between
		// can no longer be read — dead-lettered once, the cursor riding
		// the floor from then on (the lane's codex round 4; the pending
		// thread's shape).
		for ts, held := range ch.PendingMessages {
			if !held.Listing || held.FloorCrossed || floor == "" || held.Cursor == "" || compareSlackTS(held.Cursor, floor) >= 0 {
				continue
			}
			out = append(out, livenessRetiredHold{channel: id, threadTS: held.Msg.ThreadTS, cursor: held.Cursor, reason: fmt.Sprintf("inbound liveness: a thread held by its listing has replies above %s that fell below the backfill window's floor (%s) while its reads kept failing — never delivered to gc, and no longer read: read the thread from Slack and re-post them by hand; the thread stays held above the floor", held.Cursor, l.cfg.backfillMaxWindow)})
			held.FloorCrossed = true
			held.Cursor = floor
			ch.PendingMessages[ts] = held
			l.stateDirty = true
		}
		if len(ch.PendingTS) == 0 {
			continue
		}
		kept := ch.PendingTS[:0]
		var retired []string
		for _, ts := range ch.PendingTS {
			if floor == "" || compareSlackTS(ts, floor) <= 0 {
				retired = append(retired, ts)
				continue
			}
			kept = append(kept, ts)
		}
		if len(retired) == 0 {
			continue
		}
		clear(ch.PendingTS[len(kept):])
		ch.PendingTS = kept
		for _, ts := range retired {
			held, known := ch.PendingMessages[ts]
			delete(ch.PendingMessages, ts)
			out = append(out, livenessRetiredHold{channel: id, ts: ts, held: held, known: known})
		}
		ch.releaseHeldAdvanceLocked()
		l.stateDirty = true
		log.Printf("inbound liveness: channel=%s retired %d pending replay(s) out of the backfill window (%s) — never delivered to gc: going to the inbound dead letter; the watermark moves on (now %s)",
			id, len(retired), strings.Join(retired, " "), ch.WatermarkTS)
	}
	return out
}

// deadLetterRetiredHolds writes every held replay that aged out of the
// backfill window to the adapter's inbound dead letter (the designed alarm
// for a line gc never received: <city>/.gc/slack/inbound_dead_letter, the
// same file the coalescer and the receipt follow-ups use), with its text
// and thread so a hand recovery re-posts the right bytes, and says so in
// the log and on the operator channel. Nothing about a lost line is silent.
func (l *inboundLiveness) deadLetterRetiredHolds(retired []livenessRetiredHold) {
	if len(retired) == 0 {
		return
	}
	now := l.now().UTC()
	byChannel := map[string][]inboundDeadLetterRecord{}
	for _, r := range retired {
		if r.threadTS != "" {
			// A pending thread's loss: the record names the parent and the
			// cursor; there is no message text to carry.
			byChannel[r.channel] = append(byChannel[r.channel], inboundDeadLetterRecord{
				DeadLetteredAt: now,
				Channel:        r.channel,
				Reason:         r.reason,
				Inbound: externalInboundMessage{
					ProviderMessageID: r.threadTS,
					Conversation: conversationRef{
						ScopeID:        l.cfg.cityName,
						Provider:       l.cfg.provider,
						AccountID:      l.cfg.accountID,
						ConversationID: r.channel,
						Kind:           channelTypeFromID(r.channel),
					},
					ReplyToMessageID: r.threadTS,
					DedupKey:         "slack-thread-" + r.threadTS + "-" + r.cursor,
					ReceivedAt:       now,
				},
			})
			continue
		}
		reason := fmt.Sprintf("inbound liveness: held replay aged out of the backfill window (%s) after %d attempt(s) — never delivered to gc; re-post by hand", l.cfg.backfillMaxWindow, r.held.Attempts)
		switch {
		case r.held.Listing:
			reason = fmt.Sprintf("inbound liveness: a thread's listing hold aged out of the backfill window (%s) — the replies of thread %s above %s and up to this ts were never read (the pending set was full and the reads failed every probe); read them from Slack", l.cfg.backfillMaxWindow, r.held.Msg.ThreadTS, r.held.Cursor)
		case !r.known:
			reason += " (the message text was not recorded with the hold; read it from Slack at this ts)"
		case r.held.TextDropped:
			reason += " (the text was shed to fit the state file; read it from Slack at this ts)"
		}
		if r.held.FilesDropped > 0 {
			reason += fmt.Sprintf(" (%d file(s) on the message, not recorded)", r.held.FilesDropped)
		}
		team := r.held.TeamID
		if team == "" {
			team = l.cfg.accountID
		}
		byChannel[r.channel] = append(byChannel[r.channel], inboundDeadLetterRecord{
			DeadLetteredAt: now,
			Channel:        r.channel,
			Attempts:       r.held.Attempts,
			Reason:         reason,
			Inbound: externalInboundMessage{
				ProviderMessageID: r.ts,
				Conversation: conversationRef{
					ScopeID:        l.cfg.cityName,
					Provider:       l.cfg.provider,
					AccountID:      team,
					ConversationID: r.channel,
					Kind:           channelTypeFromID(r.channel),
				},
				Actor:            externalActor{ID: r.held.Msg.User, DisplayName: r.held.Msg.User},
				Text:             r.held.Msg.Text,
				ReplyToMessageID: r.held.Msg.ThreadTS,
				DedupKey:         "slack-" + r.ts,
				ReceivedAt:       now,
			},
		})
	}
	dir := strings.TrimSpace(l.cfg.inboundDeadLetterDir)
	for channel, records := range byChannel {
		if dir == "" {
			log.Printf("inbound liveness: channel=%s %d undelivered inbound record(s) LOST — no inbound dead-letter dir configured (SLACK_INBOUND_DEAD_LETTER_DIR); the lines are logged here and nowhere else", channel, len(records))
			for _, rec := range records {
				log.Printf("inbound liveness: LOST channel=%s ts=%s user=%s thread=%s text=%q reason=%q", channel, rec.Inbound.ProviderMessageID, rec.Inbound.Actor.ID, rec.Inbound.ReplyToMessageID, rec.Inbound.Text, rec.Reason)
			}
			continue
		}
		path, err := writeInboundDeadLetterRecords(dir, channel, records)
		if err != nil {
			log.Printf("inbound liveness: channel=%s dead-letter write FAILED for %d held replay(s) (dir %q): %v", channel, len(records), dir, err)
			for _, rec := range records {
				log.Printf("inbound liveness: LOST channel=%s ts=%s user=%s thread=%s text=%q", channel, rec.Inbound.ProviderMessageID, rec.Inbound.Actor.ID, rec.Inbound.ReplyToMessageID, rec.Inbound.Text)
			}
			continue
		}
		log.Printf("inbound liveness: channel=%s %d undelivered inbound record(s) dead-lettered to %s — never delivered to gc", channel, len(records), path)
		if l.alert != nil {
			go l.postAlert(fmt.Sprintf(":warning: gc-slack-adapter: %d inbound record(s) in <#%s> could not be delivered to gc within %s and went to the inbound dead letter (%s). Re-post them by hand.", len(records), channel, l.cfg.backfillMaxWindow, path))
		}
	}
}

// holdPendingLocked records a replay that did not commit, or a message a
// pass never reached (attempted says which; the dead letter counts the
// attempts): its ts joins the channel's pending set, and a watermark a live
// advance already carried past it goes back to where the probe read it
// (below every missed ts), so the next probe re-reads the message. Caller
// holds l.mu.
func (l *inboundLiveness) holdPendingLocked(m missedMessage, attempted, listing bool) (held bool) {
	ch := l.channels[m.channel]
	if ch == nil {
		ch = &livenessChannel{TeamID: m.teamID}
		l.channels[m.channel] = ch
		l.evictChannelsLocked()
	}
	ts := m.msg.TS
	if i, found := slices.BinarySearchFunc(ch.PendingTS, ts, compareSlackTS); !found {
		if len(ch.PendingTS) < livenessMaxBackfillPerChannel {
			ch.PendingTS = slices.Insert(ch.PendingTS, i, ts)
		} else if i < len(ch.PendingTS) {
			// Full: the set keeps the OLDEST outstanding ts, since the
			// watermark holds below PendingTS[0] and that guards every
			// message above it — the newest held message gives way, still
			// guarded, owned by its stretch, re-read and held again when
			// the set has room (the lane's codex round 1: a stretch read
			// below the watermark could not be held behind a full set and
			// the watermark stayed above it). A listing hold is never
			// evicted: it owns a thread's unread replies from its cursor,
			// which no stretch covers (the lane's codex round 6); a set
			// full of listing holds takes nothing more.
			victim := -1
			for j := len(ch.PendingTS) - 1; j > i; j-- {
				if !ch.PendingMessages[ch.PendingTS[j]].Listing {
					victim = j
					break
				}
			}
			if victim >= 0 {
				delete(ch.PendingMessages, ch.PendingTS[victim])
				ch.PendingTS = slices.Insert(slices.Delete(ch.PendingTS, victim, victim+1), i, ts)
			} else if listing {
				l.admitListingHoldLocked(ch, ts)
			}
		} else if listing {
			l.admitListingHoldLocked(ch, ts)
		}
	}
	if slices.Contains(ch.PendingTS, ts) {
		if ch.PendingMessages == nil {
			ch.PendingMessages = make(map[string]livenessHeldReplay)
		}
		held, known := ch.PendingMessages[ts]
		if !known {
			msg, files := clipHeldMessage(m.msg)
			held = livenessHeldReplay{Msg: msg, TeamID: m.teamID, HeldAt: l.now(), FilesDropped: files}
		}
		if attempted {
			held.Attempts++
		}
		ch.PendingMessages[ts] = held
	}
	if compareSlackTS(ch.WatermarkTS, ch.PendingTS[0]) >= 0 {
		if ch.HeldMaxTS == "" || compareSlackTS(ch.WatermarkTS, ch.HeldMaxTS) > 0 {
			ch.HeldMaxTS = ch.WatermarkTS
		}
		ch.WatermarkTS = m.watermark
	}
	l.stateDirty = true
	return slices.Contains(ch.PendingTS, ts)
}

// evictChannelsLocked drops the least recently active unpinned channels
// once the watched set exceeds livenessMaxChannels.
func (l *inboundLiveness) evictChannelsLocked() {
	for len(l.channels) > livenessMaxChannels {
		victim := ""
		var victimAt time.Time
		for id, ch := range l.channels {
			if ch.Pinned {
				continue
			}
			if victim == "" || ch.LastSeenAt.Before(victimAt) {
				victim, victimAt = id, ch.LastSeenAt
			}
		}
		if victim == "" {
			return
		}
		delete(l.channels, victim)
	}
}

// originsPutLocked records key with source, sweeping expired entries
// and enforcing the cap. Caller holds l.mu.
func (l *inboundLiveness) originsPutLocked(key string, src inboundOriginSource, now time.Time) {
	if len(l.origins) >= livenessOriginMax {
		for k, e := range l.origins {
			if now.Sub(e.at) > livenessOriginTTL {
				delete(l.origins, k)
			}
		}
		for len(l.origins) >= livenessOriginMax {
			// Still full: evict the oldest.
			oldest, oldestAt := "", now
			for k, e := range l.origins {
				if e.at.Before(oldestAt) {
					oldest, oldestAt = k, e.at
				}
			}
			if oldest == "" {
				break
			}
			delete(l.origins, oldest)
		}
	}
	l.origins[key] = originEntry{source: src, at: now}
}

// originSource reports what the tracker knows about (channel, ts).
func (l *inboundLiveness) originSource(channel, ts string) inboundOriginSource {
	if l == nil {
		return inboundOriginUnknown
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.origins[originKey(channel, ts)]
	if !ok || l.now().Sub(e.at) > livenessOriginTTL {
		return inboundOriginUnknown
	}
	return e.source
}

// onTransportConnected is called by the socket runner on every
// successful connection. It backfills the watched channels from their
// watermarks: on a reconnect that covers the socket outage; on the first
// connection it covers an adapter restart via the persisted watermarks
// (no persisted state ⇒ nothing to do).
func (l *inboundLiveness) onTransportConnected(ctx context.Context, reconnect bool) {
	if l == nil || !reconnect {
		// First connection after start: runWatchdog's startup probe
		// already covers the persisted-watermark gap.
		return
	}
	l.probe(ctx, probeModeReconnect, "socket reconnect")
}

// probeMode selects how a probe reports what it finds.
type probeMode uint8

const (
	// probeModeWatchdog: missed messages mean the live transport is not
	// delivering — ALARM, then backfill.
	probeModeWatchdog probeMode = iota
	// probeModeReconnect: missed messages were posted while the socket
	// (or the process) was down — backfill with an informational note,
	// no alarm (the transport is demonstrably back).
	probeModeReconnect
	// probeModeRetry: a held replay is due a retry while live traffic
	// flows (no stall) — backfill with an informational note, no alarm
	// and no reconnect note.
	probeModeRetry
)

// runWatchdog is the periodic stall detector. Blocks until ctx is done.
// Also owns the periodic state persistence.
func (l *inboundLiveness) runWatchdog(ctx context.Context) {
	if l == nil {
		return
	}
	// Restart gap: persisted watermarks let the adapter replay whatever
	// landed while it was down, on either transport.
	l.mu.Lock()
	hadState := !l.lastInboundAt.IsZero()
	l.mu.Unlock()
	if hadState {
		l.probe(ctx, probeModeReconnect, "startup (persisted watermarks)")
	}
	ticker := time.NewTicker(livenessTick)
	defer ticker.Stop()
	persist := time.NewTicker(livenessPersistInterval)
	defer persist.Stop()
	for {
		select {
		case <-ctx.Done():
			l.saveState()
			return
		case <-persist.C:
			l.saveState()
		case <-ticker.C:
			l.tick(ctx)
		}
	}
}

// tick runs one watchdog evaluation: probe when idle past the stall
// threshold and the probe interval has elapsed.
func (l *inboundLiveness) tick(ctx context.Context) {
	stallAfter := l.cfg.livenessStallAfter
	if stallAfter <= 0 {
		return
	}
	now := l.now()
	l.mu.Lock()
	// Silence before this process started is not this process's stall:
	// a persisted last_inbound from yesterday must not trip the alarm
	// 30s after a restart (the startup probe handles that gap).
	ref := l.lastInboundAt
	if ref.Before(l.startedAt) {
		ref = l.startedAt
	}
	idle := now.Sub(ref)
	due := idle >= stallAfter && now.Sub(l.lastProbeAt) >= livenessProbeMinInterval
	held := 0
	for _, ch := range l.channels {
		held += len(ch.PendingTS) + len(ch.PendingThreads)
		held += len(ch.PendingHistory)
	}
	retry := !due && held > 0 && l.cfg.backfillMaxWindow > 0 && now.Sub(l.lastProbeAt) >= livenessHoldRetryInterval
	l.mu.Unlock()
	switch {
	case due:
		l.probe(ctx, probeModeWatchdog, fmt.Sprintf("watchdog idle=%s", idle.Round(time.Second)))
	case retry:
		// A held replay does not wait for a stall: live traffic keeps the
		// watchdog quiet, and a hold left to the next stall, reconnect or
		// restart could age out of the window unreplayed.
		l.probe(ctx, probeModeRetry, fmt.Sprintf("held replay retry (%d held replay(s) or pending thread(s))", held))
	}
}

// probe reads every watched channel above its watermark, alarms on
// missed human messages, and (when deliverMissed) replays them. Serialized:
// a probe already in flight makes this call a no-op.
func (l *inboundLiveness) probe(ctx context.Context, mode probeMode, reason string) {
	if l == nil || l.fetch == nil {
		return
	}
	deliverMissed := l.cfg.backfillMaxWindow > 0
	l.mu.Lock()
	if l.probing {
		l.mu.Unlock()
		return
	}
	l.probing = true
	l.lastProbeAt = l.now()
	retired := l.retireExpiredPendingLocked(l.lastProbeAt)
	channels := l.snapshotChannelsLocked()
	// This probe reads the stretches as they stand: what was added into
	// them before now is part of the read, not of what this probe's
	// settle must keep.
	for _, ch := range l.channels {
		for i := range ch.PendingHistory {
			ch.PendingHistory[i].addedFrom, ch.PendingHistory[i].addedBelow = "", ""
		}
	}
	l.mu.Unlock()
	defer func() {
		l.mu.Lock()
		l.probing = false
		l.mu.Unlock()
	}()
	l.deadLetterRetiredHolds(retired)
	l.probesTotal.Add(1)
	if len(channels) == 0 {
		log.Printf("inbound liveness: probe (%s) skipped — no watched channels yet (pin with SLACK_LIVENESS_CHANNELS)", reason)
		return
	}

	var missed []missedMessage
	var skipped []string
	var reads []threadRead           // the thread reads of this probe, settled after the backfill
	settles := map[string][]func(){} // the unread-stretch updates per channel, applied after the backfill
	for _, c := range channels {
		found, threadReads, stretchSettles, err := l.probeChannel(ctx, c.id, c.ch)
		if err != nil {
			skipped = append(skipped, c.id+":"+scrubSlackSecrets(err.Error()))
			continue
		}
		missed = append(missed, found...)
		reads = append(reads, threadReads...)
		settles[c.id] = stretchSettles
	}
	// A message read twice this probe — from a stretch and above the
	// watermark, say — is one missed message: counted once, replayed once,
	// charged to the cap once (the lane's codex round 8).
	missed = dedupeMissed(missed)
	l.lastMissed.Store(int64(len(missed)))
	if len(skipped) > 0 {
		log.Printf("inbound liveness: probe (%s) skipped %d channel(s): %s", reason, len(skipped), strings.Join(skipped, "; "))
	}
	if len(missed) == 0 {
		l.settlePendingThreads(reads, nil, nil)
		l.settleStretches(settles)
		log.Printf("inbound liveness: probe (%s) clean — %d channel(s) checked, nothing missed", reason, len(channels))
		return
	}
	l.missedTotal.Add(uint64(len(missed)))
	sort.Slice(missed, func(i, j int) bool { return compareSlackTS(missed[i].msg.TS, missed[j].msg.TS) < 0 })
	byChannel := map[string]int{}
	for _, m := range missed {
		byChannel[m.channel]++
	}
	if mode == probeModeWatchdog {
		l.alarm(reason, missed, byChannel)
	} else {
		log.Printf("inbound liveness: %s — %d message(s) landed while inbound was down (%s); replaying",
			reason, len(missed), summarizeByChannel(byChannel))
	}
	if !deliverMissed || l.deliver == nil {
		log.Printf("inbound liveness: backfill disabled — %d missed message(s) NOT replayed", len(missed))
		return
	}
	delivered, incomplete := l.backfill(ctx, missed)
	l.settlePendingThreads(reads, missed, incomplete)
	l.settleStretches(settles)
	log.Printf("inbound liveness: backfill (%s) replayed %d/%d missed message(s)", reason, delivered, len(missed))
	if mode == probeModeReconnect && delivered > 0 && l.alert != nil {
		go l.postAlert(fmt.Sprintf(":arrows_counterclockwise: gc-slack-adapter backfilled %d message(s) that landed while inbound was down (%s; %s).",
			delivered, reason, summarizeByChannel(byChannel)))
	}
}

// dedupeMissed keeps the first of every (channel, ts) in missed.
func dedupeMissed(missed []missedMessage) []missedMessage {
	seen := make(map[string]bool, len(missed))
	out := missed[:0]
	for _, m := range missed {
		key := originKey(m.channel, m.msg.TS)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, m)
	}
	return out
}

// settleStretches applies the probe's unread-stretch shrinks and drops, per
// channel in order, once the backfill has delivered or held every message
// read from the stretches (every message a pass leaves is held: finding
// 3n). A stretch a probe leaves is installed at once, not here.
func (l *inboundLiveness) settleStretches(settles map[string][]func()) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for channel, fs := range settles {
		if l.channels[channel] == nil {
			continue
		}
		for _, f := range fs {
			f()
		}
	}
}

// summarizeByChannel renders {channel: count} as "C1:2 C2:1", sorted.
func summarizeByChannel(byChannel map[string]int) string {
	parts := make([]string, 0, len(byChannel))
	for c, n := range byChannel {
		parts = append(parts, fmt.Sprintf("%s:%d", c, n))
	}
	sort.Strings(parts)
	return strings.Join(parts, " ")
}

type watchedChannel struct {
	id string
	ch livenessChannel
}

// snapshotChannelsLocked copies the watched set, most recently active
// first. Caller holds l.mu.
func (l *inboundLiveness) snapshotChannelsLocked() []watchedChannel {
	out := make([]watchedChannel, 0, len(l.channels))
	for id, ch := range l.channels {
		c := *ch
		c.PendingTS = slices.Clone(ch.PendingTS)
		c.PendingThreads = slices.Clone(ch.PendingThreads)
		c.PendingMessages = maps.Clone(ch.PendingMessages)
		c.PendingHistory = slices.Clone(ch.PendingHistory)
		out = append(out, watchedChannel{id: id, ch: c})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ch.Pinned != out[j].ch.Pinned {
			return out[i].ch.Pinned
		}
		return out[i].ch.LastSeenAt.After(out[j].ch.LastSeenAt)
	})
	return out
}

// probeChannel reads one channel above its watermark (bounded by the
// backfill window) and returns the human messages the adapter never
// saw, including fresh thread replies.
func (l *inboundLiveness) probeChannel(ctx context.Context, channel string, ch livenessChannel) (missed []missedMessage, reads []threadRead, settles []func(), err error) {
	now := l.now()
	oldest := ch.WatermarkTS
	floor := "" // the backfill window's floor: nothing is read below it
	if win := l.cfg.backfillMaxWindow; win > 0 {
		floor = slackTSFromTime(now.Add(-win))
		if oldest == "" || compareSlackTS(oldest, floor) < 0 {
			oldest = floor
		}
	} else if oldest == "" {
		oldest = slackTSFromTime(now.Add(-l.cfg.livenessStallAfter - time.Minute))
	}
	// The listed threads' cursors: a thread listed from the pending
	// history stretch reads from that stretch's floor, the rest from the
	// watermark.
	threadFrom := map[string]string{}
	var threads []slackHistoryMessage
	// collect takes one stretch of history read above lower: parents
	// whose latest_reply moved past lower point at threads with fresh
	// replies (history only lists parents by their own ts, so a reply
	// posted during the gap into an older thread is otherwise invisible);
	// messages above lower the adapter never saw are missed.
	collect := func(msgs []slackHistoryMessage, lower, watermark string) {
		for _, m := range msgs {
			if m.ReplyCount > 0 && m.LatestReply != "" && compareSlackTS(m.LatestReply, lower) > 0 {
				if _, seen := threadFrom[m.TS]; !seen {
					threadFrom[m.TS] = lower
					threads = append(threads, m)
				}
			}
			if compareSlackTS(m.TS, lower) <= 0 {
				continue
			}
			if l.originSource(channel, m.TS) != inboundOriginUnknown || !historyMessageDispatchable(m) {
				continue
			}
			missed = append(missed, missedMessage{channel: channel, teamID: ch.TeamID, msg: m, watermark: watermark})
		}
	}
	// The stretches a previous probe could not finish reading come first
	// (gf-d7b finding 3o): below Below, page by page, down to From; a
	// stretch shrinks with every page read and is dropped once Slack has
	// no more — after the backfill — and the messages a pass leaves in it
	// stay pending as a stretch of their own.
	// The stretch updates are applied in order, under the lock, by probe
	// — only once every read of this probe succeeded (a failed first page
	// discards everything this probe collected, the stretch's messages
	// included, and the stretch must still stand for the next probe; the
	// lane's codex round 1) and only after the backfill delivered or held
	// every message of the stretch (a save between the update and the
	// delivery, from a reconnect probe beside the watchdog's persist, would
	// otherwise leave messages below the watermark with no recovery owner
	// across a restart; the lane's codex round 2).
	budget := livenessMaxHistoryPages // history pages this probe, this channel
	pagesRead := 0
	for _, ph := range ch.PendingHistory {
		if budget == 0 {
			break
		}
		from, below := ph.From, ph.Below
		if floor != "" && compareSlackTS(from, floor) < 0 {
			from = floor
		}
		done, pages, failed := false, 0, false
		for budget > 0 {
			msgs, more, err := l.fetchHistoryPage(ctx, channel, from, below, livenessHistoryLimit)
			budget--
			if err != nil {
				if pagesRead == 0 {
					return nil, nil, nil, err
				}
				// A later page failed: what was read stands, the stretch
				// shrinks to it, and the next probe goes on from there
				// (the lane's codex round 2).
				log.Printf("inbound liveness: channel=%s unread history page below %s: %v — the %d page(s) read stand; the stretch goes on from there", channel, below, scrubSlackSecrets(err.Error()), pagesRead)
				failed = true
				break
			}
			// The stretch owns what it holds: no pull-back of the
			// watermark for a hold on one of its messages (the lane's
			// codex round 5: the pull-back made the read above the
			// watermark cover the stretch again, every probe).
			collect(msgs, from, ch.WatermarkTS)
			pages++
			pagesRead++
			if !more {
				done = true
				break
			}
			if len(msgs) == 0 {
				// Slack has more but gave nothing usable this page (the
				// client drops entries without a ts): not a read-through —
				// the stretch stands as it was read and the next probe
				// goes on (the lane's codex round 10).
				log.Printf("inbound liveness: channel=%s an empty unread-history page below %s with more to come; the stretch stands", channel, below)
				break
			}
			below = msgs[0].TS
		}
		if pages > 0 {
			readFrom, readBelow, readTo, readDone := ph.From, ph.Below, below, done
			settles = append(settles, func() { l.settleStretchLocked(channel, readFrom, readBelow, readTo, readDone) })
			if compareSlackTS(from, ph.From) > 0 {
				// The floor entered the stretch after this probe's retire
				// pass (earlier channels took that long): the prefix under
				// the floor was not read, and the settle above must not
				// drop it as read — it stands, and the next probe's retire
				// pass reports it (the lane's codex round 9).
				prefixFrom, prefixBelow := ph.From, slackTSAfter(from)
				settles = append(settles, func() {
					if l.channels[channel] != nil {
						l.addStretchLocked(channel, prefixFrom, prefixBelow, false)
					}
				})
			}
		}
		if failed {
			break
		}
	}
	// Then the channel above its watermark, page by page while the pages
	// are still above it (gf-d7b finding 3o: one page used to be read and
	// has_more dropped, so the newest 200 of a longer gap replayed and the
	// watermark moved past the rest). Pages below the watermark are the
	// thread scan's: the read reaches back livenessThreadScanWindow for
	// parents whose latest_reply moved past the watermark, and one page's
	// worth of that is the best effort, as before. Pages above the
	// watermark a probe cannot finish are pending on the channel from the
	// oldest ts it did read down to the watermark — installed at once —
	// and the next probe reads them first.
	fetchFrom := oldest
	if scanOldest := slackTSFromTime(now.Add(-livenessThreadScanWindow)); compareSlackTS(scanOldest, oldest) < 0 {
		fetchFrom = scanOldest
	}
	leave := func(below string) {
		l.mu.Lock()
		if l.channels[channel] != nil {
			l.addStretchLocked(channel, oldest, below, false)
		}
		l.mu.Unlock()
	}
	latest := ""
	top := "" // the newest message read above the watermark
	for page := 0; ; page++ {
		msgs, more, err := l.fetchHistoryPage(ctx, channel, fetchFrom, latest, livenessHistoryLimit)
		if err != nil {
			if page == 0 {
				if pagesRead == 0 {
					return nil, nil, nil, err
				}
				// The stretches were read and the read above the watermark
				// failed: what the stretches found stands and replays (a
				// budget of one request per probe would otherwise re-read
				// the same stretch forever; the lane's codex round 3); the
				// channel above its watermark waits for the next probe.
				log.Printf("inbound liveness: channel=%s history above the watermark %s: %v — the %d unread-history page(s) read stand; the channel above its watermark waits for the next probe", channel, oldest, scrubSlackSecrets(err.Error()), pagesRead)
				break
			}
			// A later page failed: the pages read stand and replay, and
			// the pages below the oldest ts read are pending — the next
			// probe reads them first (the lane's codex round 2: a read
			// that failed whole on any page replayed nothing, and a rate
			// budget that allowed one page and refused the next starved
			// the channel until the window closed).
			leave(latest)
			log.Printf("inbound liveness: channel=%s history page %d below %s: %v — the %d page(s) read stand; the pages below %s are pending and the next probe reads them first", channel, page+1, latest, scrubSlackSecrets(err.Error()), page, latest)
			break
		}
		collect(msgs, oldest, ch.WatermarkTS)
		if top == "" && len(msgs) > 0 && compareSlackTS(msgs[len(msgs)-1].TS, oldest) > 0 {
			top = msgs[len(msgs)-1].TS
		}
		if !more {
			break
		}
		if len(msgs) == 0 {
			// More to come but nothing usable this page: what lies below
			// the last page read is pending, as after a failed later page
			// (the lane's codex round 10).
			if latest != "" {
				leave(latest)
			}
			log.Printf("inbound liveness: channel=%s an empty history page below %q with more to come; the pages below are pending", channel, latest)
			break
		}
		if compareSlackTS(msgs[0].TS, oldest) <= 0 {
			break
		}
		latest = msgs[0].TS // oldest-first: the page below starts under it
		if page+1 >= livenessMaxHistoryPages {
			leave(latest)
			log.Printf("inbound liveness: channel=%s has more than %d pages of history above its watermark %s; the pages below %s are pending and the next probe reads them first", channel, livenessMaxHistoryPages, oldest, latest)
			break
		}
	}
	// The threads this channel still owes a read (gf-c7n finding 3i) are
	// read from their own cursor whether or not the listing above names
	// them: a failed read left their replies unknown while the channel
	// messages found beside them replayed and the watermark moved on, so
	// latest_reply alone would never list them again. A listed thread that
	// is pending reads from its cursor too; its pending state is settled
	// after the backfill (settlePendingThreads).
	cursor := map[string]string{}
	for _, p := range ch.PendingThreads {
		cursor[p.ParentTS] = p.Oldest
	}
	// A thread under a listing hold (finding 3m) reads from the hold's
	// cursor too: the hold keeps the watermark below the thread's latest
	// reply, but a replay or a live message below that reply still moves
	// the watermark past older unread replies (the lane's codex round 2).
	for _, held := range ch.PendingMessages {
		if !held.Listing || held.Msg.ThreadTS == "" || held.Cursor == "" {
			continue
		}
		if c, ok := cursor[held.Msg.ThreadTS]; !ok || compareSlackTS(held.Cursor, c) < 0 {
			cursor[held.Msg.ThreadTS] = held.Cursor
		}
	}
	if top != "" {
		// Everything between the watermark and the newest message read is
		// now read: delivered, known, not dispatchable, held with its
		// stretch, or left as a stretch. The watermark moves up to it after
		// the backfill (touchChannelLocked: a hold keeps it, remembering
		// the advance), so the next probe does not read, and leave again,
		// what this one read — a bot-only backlog above an unmoved
		// watermark re-registered its drained pages every probe (the
		// lane's codex round 8).
		settles = append(settles, func() { l.advanceWatermarkLocked(channel, top) })
	}
	// Most recently active threads first; cap reads per probe.
	sort.Slice(threads, func(i, j int) bool { return compareSlackTS(threads[i].LatestReply, threads[j].LatestReply) > 0 })
	if len(threads) > livenessMaxThreadsPerChannel {
		// Every thread the cap cuts is PENDING from this probe's reading
		// BEFORE the cut (gf-d7b finding 3m): the replies replayed from the
		// threads that are read, and the live traffic, move the watermark
		// past the cut threads' latest_reply, and the listing above would
		// never name them again. The next probe reads each from its cursor
		// (the pending loop below, which reads from the snapshot's pending
		// set, takes them from the next probe on). One the pending set
		// cannot take is read THIS probe instead — the cap is soft for it:
		// a hold below its latest reply would keep it listed, but the ten
		// busier threads would be read ahead of it every probe for as long
		// as they stay busier, and nothing else could ever reach it.
		cut := threads[livenessMaxThreadsPerChannel:]
		kept := threads[:livenessMaxThreadsPerChannel:livenessMaxThreadsPerChannel]
		pending := 0
		l.mu.Lock()
		for _, t := range cut {
			from := oldest
			if c, ok := threadFrom[t.TS]; ok {
				from = c // a thread listed from the pending stretch: its floor
			}
			if c, ok := cursor[t.TS]; ok && compareSlackTS(c, from) < 0 {
				from = c // a listing hold's cursor: below the replies it owes (the lane's codex round 7)
			}
			if l.notePendingThreadLocked(channel, ch.TeamID, t.TS, from) {
				pending++
				continue
			}
			kept = append(kept, t)
		}
		l.mu.Unlock()
		log.Printf("inbound liveness: channel=%s has %d threads with fresh replies; reading the %d most recent this probe, %d pending for the next from where this probe read, %d read now because the pending set is full", channel, len(threads), livenessMaxThreadsPerChannel, pending, len(kept)-livenessMaxThreadsPerChannel)
		threads = kept
	}
	owed := map[string]string{} // parent ts → the latest reply its listing hold is keyed by, if any
	for _, p := range ch.PendingThreads {
		owed[p.ParentTS] = ""
	}
	// A thread under a listing hold is owed a read the same way: the
	// listing names it only while its parent is on the page the scan
	// reaches, and newer channel messages push the parent past it (the
	// lane's codex round 6). Its entry carries the hold's key, so a read
	// that delivers a page and finds more moves the hold's cursor (the
	// lane's codex round 7).
	for ts, held := range ch.PendingMessages {
		if held.Listing && held.Msg.ThreadTS != "" {
			owed[held.Msg.ThreadTS] = ts
		}
	}
	listedCount := len(threads)
	for parentTS, latest := range owed {
		listed := false
		for _, t := range threads[:listedCount] {
			if t.TS == parentTS {
				listed = true
				break
			}
		}
		if !listed {
			threads = append(threads, slackHistoryMessage{TS: parentTS, LatestReply: latest})
		}
	}
	appended := threads[listedCount:]
	sort.Slice(appended, func(i, j int) bool { return compareSlackTS(appended[i].TS, appended[j].TS) < 0 })
	for _, p := range threads {
		// A pending thread reads from its own cursor, below the watermark
		// if that is where its undelivered replies are, never below the
		// window's floor.
		from := oldest
		if c, ok := threadFrom[p.TS]; ok {
			from = c
		}
		if c, ok := cursor[p.TS]; ok {
			from = c
			if floor != "" && compareSlackTS(floor, from) > 0 {
				from = floor
			}
		}
		rctx, rcancel := context.WithTimeout(ctx, livenessFetchTimeout)
		replies, more, err := l.fetch.replies(rctx, channel, p.TS, from, livenessHistoryLimit)
		rcancel()
		if err != nil {
			// Its fresh replies are unknown: the thread is pending from
			// this read's cursor, and the next probe reads it again.
			log.Printf("inbound liveness: thread read channel=%s ts=%s: %v — its replies above %s are unknown; pending until a read succeeds", channel, p.TS, scrubSlackSecrets(err.Error()), from)
			l.mu.Lock()
			if !l.notePendingThreadLocked(channel, ch.TeamID, p.TS, from) {
				// From the read's own cursor (the lane's codex round 3): a
				// thread listed from a stretch reads below the watermark.
				l.holdThreadListingLocked(channel, ch.TeamID, p.TS, p.LatestReply, from, false)
			}
			l.mu.Unlock()
			continue
		}
		read := threadRead{channel: channel, threadTS: p.TS, oldest: from, complete: !more, latestReply: p.LatestReply}
		for _, m := range replies {
			if m.TS == p.TS || compareSlackTS(m.TS, from) <= 0 {
				continue
			}
			read.fetched = append(read.fetched, m.TS)
			if l.originSource(channel, m.TS) != inboundOriginUnknown || !historyMessageDispatchable(m) {
				continue
			}
			missed = append(missed, missedMessage{channel: channel, teamID: ch.TeamID, msg: m, watermark: ch.WatermarkTS})
		}
		reads = append(reads, read)
	}
	return missed, reads, settles, nil
}

// historyMessageDispatchable mirrors processSlackEvent's admission
// filter: a human-authored plain message (or file_share) with text or
// files. Anything the live path would have dropped is not "missed".
func historyMessageDispatchable(m slackHistoryMessage) bool {
	if m.Type != "" && m.Type != "message" {
		return false
	}
	if m.BotID != "" || (m.Subtype != "" && m.Subtype != "file_share") || m.User == "" {
		return false
	}
	return strings.TrimSpace(m.Text) != "" || len(m.Files) > 0
}

// alarm records and announces a confirmed inbound stall.
func (l *inboundLiveness) alarm(reason string, missed []missedMessage, byChannel map[string]int) {
	now := l.now()
	l.alarmsTotal.Add(1)
	l.mu.Lock()
	first := l.stalledSince.IsZero()
	if first {
		l.stalledSince = now
	}
	ref := l.lastInboundAt
	if ref.IsZero() {
		ref = l.startedAt
	}
	idle := now.Sub(ref)
	shouldPost := l.alert != nil && (l.lastAlertAt.IsZero() || now.Sub(l.lastAlertAt) >= livenessAlertRepeat)
	if shouldPost {
		l.lastAlertAt = now
		l.alerted = true
	}
	l.mu.Unlock()

	summary := summarizeByChannel(byChannel)
	oldest, newest := missed[0].msg.TS, missed[len(missed)-1].msg.TS
	log.Printf("INBOUND LIVENESS ALARM: no live inbound for %s but %d new human message(s) in channel history (%s) oldest_ts=%s newest_ts=%s — inbound transport is NOT delivering (%s)",
		idle.Round(time.Second), len(missed), summary, oldest, newest, reason)
	// Escalation (gp-bsk): the alarm is positive evidence the transport
	// is not delivering — flip a down socket runner into aggressive
	// reconnect (backoff floor, abandon any backoff sleep) instead of
	// letting it wait out a multi-hour ladder. Nil-safe on both ends.
	socketModeHealth.Load().onInboundAlarm()
	if shouldPost {
		text := fmt.Sprintf(":rotating_light: gc-slack-adapter INBOUND LIVENESS ALARM — no live inbound event for %s, but %d new human message(s) are in channel history (%s). The inbound transport is not delivering. %s",
			idle.Round(time.Second), len(missed), summary, l.transportHint())
		go l.postAlert(text)
	}
}

// transportHint summarizes which inbound transport is configured, for
// the alarm text.
func (l *inboundLiveness) transportHint() string {
	if r := socketModeHealth.Load(); r != nil {
		if r.connected.Load() {
			return "Socket Mode reports connected (check the app's event subscriptions / bot membership)."
		}
		return "Socket Mode is NOT connected (see socket_last_error on /healthz)."
	}
	return "Transport is the Events API Request URL (funnel) — check Slack → Event Subscriptions and the public listener."
}

// postAlert sends one line to the operator channel; errors are logged.
func (l *inboundLiveness) postAlert(text string) {
	if l.alert == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := l.alert(ctx, text); err != nil {
		log.Printf("inbound liveness: alert post failed: %v", scrubSlackSecrets(err.Error()))
	}
}

// livenessBackfillSettleWait bounds how long a replay waits for its
// dispatch to conclude (the gc forward's own timeout is 20 s; the
// mention-only confirm hold 10 s) before it is treated as not delivered.
// A variable so tests can shorten it.
var livenessBackfillSettleWait = 30 * time.Second

// backfill replays missed messages, oldest first, through the events
// pipeline as synthesized envelopes. Each origin is marked backfilled
// BEFORE delivery so a racing live copy is dropped. The recovery state —
// that marker and the channel's watermark advance — COMMITS only once the
// replay's dispatch concluded committed: delivered to gc, or durably queued
// with retry ownership (backfillCommitted; gf-xid finding 3g — the handler's
// immediate 200 precedes the forward, and a replay whose gc POST then failed
// used to keep its marker and its watermark, so the live redelivery was
// dropped as a duplicate and every later backfill skipped it). A replay that
// did not commit is un-marked so a live copy can still land, and its ts is
// held pending on the channel (holdPendingLocked): the watermark does not
// cross it until it lands, so the next probe re-reads it, while every later
// message that did commit here, or arrived live, is skipped by its known
// origin. (The lane's codex round 1 on the first cut: a channel that HALTED
// at its first failure left its newer missed messages unreplayed, and a
// live message that advanced the watermark meanwhile put them below it for
// good; the pass now replays everything it can and the watermark holds
// instead.) A message the per-channel cap or a cancellation leaves
// unreplayed is HELD pending on its channel exactly as a failed replay is
// (holdPendingLocked, not an attempt; gf-d7b finding 3n): the watermark does
// not cross it, the retry timer re-reads it under live traffic, and the
// next probe replays it — before, it was left un-marked and not pending,
// and a live message that advanced the watermark past it excluded it from
// every later probe for good. The pending set holds at most
// livenessMaxBackfillPerChannel of them; the watermark holds below the
// oldest, which guards every one above it, and the next pass holds what it
// still cannot reach. Returns the count delivered and the channels this
// pass left INCOMPLETE, so a thread a left message belongs to stays pending
// (settlePendingThreads).
func (l *inboundLiveness) backfill(ctx context.Context, missed []missedMessage) (int, map[string]bool) {
	perChannel := map[string]int{}
	delivered := 0
	incomplete := map[string]bool{}
	left := map[string]int{}
	leftRange := map[string][2]string{} // per channel: the ts range a pass left, both ends exclusive
	// leaveLocked holds a message this pass did not deliver — never
	// reached (attempted=false) or failed (attempted=true) — and gives it
	// its owner at once, whatever the hold set does: the range this pass
	// has left so far is an unread stretch — the holds alone did not keep
	// it (live copies of every held message released them and the held
	// advance carried the watermark past the rest; an older message's hold
	// evicts a newer one's; a failed replay of a message read from a
	// stretch lies below the watermark the read above it starts at; the
	// lane's codex rounds 4 to 6). The stretch is read first by the next
	// probe, whatever the watermark does. Caller holds l.mu.
	leaveLocked := func(m missedMessage, attempted bool) {
		l.holdPendingLocked(m, attempted, false)
		r := leftRange[m.channel]
		if r[0] == "" || compareSlackTS(m.msg.TS, r[0]) <= 0 {
			r[0] = slackTSBefore(m.msg.TS)
		}
		if r[1] == "" || compareSlackTS(m.msg.TS, r[1]) >= 0 {
			r[1] = slackTSAfter(m.msg.TS)
		}
		leftRange[m.channel] = r
		if l.channels[m.channel] != nil {
			l.addStretchLocked(m.channel, r[0], r[1], true)
		}
	}
	for _, m := range missed {
		if ctx.Err() != nil || perChannel[m.channel] >= livenessMaxBackfillPerChannel {
			incomplete[m.channel] = true
			l.mu.Lock()
			// A live copy that landed since the probe read it needs no
			// hold: nothing would release one (the lane's codex round 1).
			if e, ok := l.origins[originKey(m.channel, m.msg.TS)]; !ok || e.source == inboundOriginUnknown {
				left[m.channel]++
				leaveLocked(m, false)
			}
			l.mu.Unlock()
			continue
		}
		perChannel[m.channel]++
		env, err := l.synthesizeEnvelope(m)
		if err != nil {
			log.Printf("inbound liveness: backfill synthesize channel=%s ts=%s: %v", m.channel, m.msg.TS, err)
			continue
		}
		key := originKey(m.channel, m.msg.TS)
		now := l.now()
		l.mu.Lock()
		if e, ok := l.origins[key]; ok && e.source != inboundOriginUnknown {
			// A live copy landed between probe and replay.
			l.mu.Unlock()
			continue
		}
		l.originsPutLocked(key, inboundOriginBackfilled, now)
		if l.inFlight == nil {
			l.inFlight = make(map[string]bool)
		}
		l.inFlight[key] = true
		l.mu.Unlock()
		outcome := l.deliver(ctx, env)
		if ok, why := l.backfillCommitted(ctx, outcome); !ok {
			l.mu.Lock()
			delete(l.origins, key)
			delete(l.inFlight, key)
			leaveLocked(m, true)
			l.mu.Unlock()
			log.Printf("inbound liveness: backfill delivery channel=%s ts=%s %s — left for a live redelivery or the next probe: the origin is unmarked and the channel's watermark holds below it", m.channel, m.msg.TS, why)
			continue
		}
		delivered++
		l.backfilledTotal.Add(1)
		l.mu.Lock()
		delete(l.inFlight, key)
		l.touchChannelLocked(m.channel, m.teamID, m.msg.TS, now)
		l.mu.Unlock()
		log.Printf("inbound liveness: backfilled channel=%s ts=%s user=%s thread=%s text=%dch", m.channel, m.msg.TS, m.msg.User, m.msg.ThreadTS, len(m.msg.Text))
	}
	for channel, n := range left {
		why := fmt.Sprintf("the per-channel cap of %d", livenessMaxBackfillPerChannel)
		if ctx.Err() != nil {
			why = "a cancellation"
		}
		r := leftRange[channel]
		log.Printf("inbound liveness: backfill channel=%s left %d missed message(s) unreplayed this pass (%s) — held pending, and (%s, %s) pending as an unread stretch: the next probe replays them", channel, n, why, r[0], r[1])
	}
	return delivered, incomplete
}

// backfillCommitted decides whether a replay's recovery state may commit:
// the handler accepted the envelope AND the dispatch it took concluded
// committed — delivered to gc, or durably queued with retry ownership — or
// the handler took no dispatch claim at all and its status is the whole
// answer (a deliver hook that runs no handler answers a nil verdict for the
// same reason). Bounded by livenessBackfillSettleWait and ctx: a dispatch
// still in flight at the bound is NOT committed. That errs toward a
// duplicate — the event dedup parks a later replay behind the in-flight copy
// and drops it on a commit, and gc dedups a channel copy by its message ts —
// and never toward a silent loss, the trade this codebase always takes.
func (l *inboundLiveness) backfillCommitted(ctx context.Context, outcome backfillDelivery) (bool, string) {
	if !outcome.accepted() {
		return false, fmt.Sprintf("status=%d", outcome.status)
	}
	if outcome.verdict == nil {
		return true, ""
	}
	sctx, cancel := context.WithTimeout(ctx, livenessBackfillSettleWait)
	defer cancel()
	committed, settled, watched := outcome.verdict.settle(sctx)
	switch {
	case !watched:
		return true, ""
	case !settled:
		return false, fmt.Sprintf("dispatch not concluded within %s", livenessBackfillSettleWait)
	case !committed:
		return false, "forward to gc failed (the dispatch claim was forgotten)"
	}
	return true, ""
}

// synthesizeEnvelope builds the event_callback envelope the Events API
// would have delivered for a history message: the raw message object
// plus the fields history omits (channel, channel_type, event_ts),
// wrapped with the team/app ids and the bot's authorization so bot-
// mention detection works.
func (l *inboundLiveness) synthesizeEnvelope(m missedMessage) (slackEventEnvelope, error) {
	var obj map[string]any
	if len(m.msg.Raw) > 0 {
		if err := json.Unmarshal(m.msg.Raw, &obj); err != nil {
			return slackEventEnvelope{}, fmt.Errorf("decode history message: %w", err)
		}
	}
	if obj == nil {
		obj = map[string]any{}
	}
	obj["type"] = "message"
	obj["channel"] = m.channel
	if _, ok := obj["channel_type"]; !ok {
		obj["channel_type"] = channelTypeFromID(m.channel)
	}
	if _, ok := obj["event_ts"]; !ok {
		obj["event_ts"] = m.msg.TS
	}
	raw, err := json.Marshal(obj)
	if err != nil {
		return slackEventEnvelope{}, fmt.Errorf("encode synthesized event: %w", err)
	}
	l.mu.Lock()
	team := m.teamID
	if team == "" {
		team = l.teamID
	}
	app := l.apiAppID
	bot := l.botUserID
	l.mu.Unlock()
	env := slackEventEnvelope{
		Type:     "event_callback",
		TeamID:   team,
		APIAppID: app,
		EventID:  "backfill:" + m.channel + ":" + m.msg.TS,
		Event:    raw,
	}
	if bot != "" {
		env.Authorizations = []slackEventAuthorization{{UserID: bot, IsBot: true}}
	}
	return env, nil
}

// channelTypeFromID maps a Slack conversation id prefix to the
// channel_type the Events API would carry.
func channelTypeFromID(id string) string {
	if id == "" {
		return ""
	}
	switch id[0] {
	case 'C':
		return "channel"
	case 'D':
		return "im"
	case 'G':
		return "group"
	}
	return ""
}

// deliverViaHandler adapts an events handler into the deliver hook:
// marshal the envelope, build the trusted "backfill" request carrying a
// fresh backfillVerdict, run the handler in-memory, return its status and
// the verdict the handler attached to the event's dispatch claim (gf-xid
// finding 3g).
func deliverViaHandler(h http.Handler) func(ctx context.Context, env slackEventEnvelope) backfillDelivery {
	return func(ctx context.Context, env slackEventEnvelope) backfillDelivery {
		body, err := json.Marshal(env)
		if err != nil {
			return backfillDelivery{status: http.StatusInternalServerError}
		}
		verdict := &backfillVerdict{}
		rctx := withBackfillVerdict(withTrustedTransport(ctx, transportBackfill), verdict)
		req, _ := http.NewRequestWithContext(rctx, http.MethodPost, "/slack/events", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.ContentLength = int64(len(body))
		req.RemoteAddr = transportBackfill
		w := newMemResponseWriter()
		h.ServeHTTP(w, req)
		return backfillDelivery{status: w.Status(), verdict: verdict}
	}
}

// slackAlertPoster returns the alert hook: chat.postMessage to channel
// with the bot token.
func slackAlertPoster(token, channel string) func(ctx context.Context, text string) error {
	if token == "" || channel == "" {
		return nil
	}
	return func(ctx context.Context, text string) error {
		_ = ctx
		resp, err := postToSlack(token, slackPostMessageReq{Channel: channel, Text: text})
		if err != nil {
			return err
		}
		if !resp.OK {
			return fmt.Errorf("chat.postMessage: %s", resp.Error)
		}
		return nil
	}
}

// loadState reads the persisted watermarks. Any failure logs and starts
// fresh — the file is an optimization (restart backfill), never a gate.
func (l *inboundLiveness) loadState() {
	if l.statePath == "" {
		return
	}
	data, err := os.ReadFile(l.statePath)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			log.Printf("inbound liveness: state %s unreadable (%v) — starting fresh", l.statePath, err)
		}
		return
	}
	if len(data) > livenessStateMaxBytes {
		log.Printf("inbound liveness: state %s oversized (%d bytes) — starting fresh", l.statePath, len(data))
		return
	}
	var st livenessState
	if err := json.Unmarshal(data, &st); err != nil {
		log.Printf("inbound liveness: state %s corrupt (%v) — starting fresh", l.statePath, err)
		return
	}
	l.lastInboundAt = st.LastInboundAt
	now := l.now()
	restored := 0
	for id, ch := range st.Channels {
		if id == "" || ch == nil {
			continue
		}
		ch.Pinned = false // re-derived from config each start
		// The delivered record of a held channel comes back with the hold,
		// so the startup probe replays only what was never delivered.
		for _, o := range ch.HeldOrigins {
			if o.TS == "" {
				continue
			}
			src := inboundOriginBackfilled
			if o.Live {
				src = inboundOriginLive
			}
			key := originKey(id, o.TS)
			l.originsPutLocked(key, src, now)
			// Its presence in the file is the proof it was delivered: the
			// latch comes back with it, so the next save under a hold
			// carries it again (the lane's codex round 8).
			if e, ok := l.origins[key]; ok {
				e.delivered = true
				l.origins[key] = e
			}
			restored++
		}
		ch.HeldOrigins = nil
		l.channels[id] = ch
	}
	if restored > 0 {
		log.Printf("inbound liveness: restored %d delivered origin(s) of held channel(s) from %s", restored, l.statePath)
	}
	log.Printf("inbound liveness: loaded state %s (channels=%d last_inbound=%s)", l.statePath, len(l.channels), st.LastInboundAt.UTC().Format(time.RFC3339))
}

// liveDeliveredLocked reports whether a live origin's line reached gc (or
// durable retry ownership): its event's dedup claim concluded committed.
// The note precedes the claim and the forward, so a line still buffered or
// forwarding is not delivered yet, and a crash before it concludes must
// leave it for the recovery to replay (the lane's codex round 7). The
// conclusion latches on the entry once seen. Without the claim machinery
// (no dedup cache: tests) a noted line counts as delivered. Caller holds
// l.mu.
func (l *inboundLiveness) liveDeliveredLocked(key string, e originEntry) bool {
	if e.delivered {
		return true
	}
	if l.cfg.eventDedup == nil {
		return true
	}
	if e.eventID == "" || !l.cfg.eventDedup.committed(e.eventID) {
		return false
	}
	e.delivered = true
	l.origins[key] = e
	return true
}

// heldOriginsLocked lists the origins channel knows above its watermark —
// a replay that concluded delivered, or a live message — oldest first, for
// the state file; a replay still in flight is left out, and so is a live
// line whose forward has not concluded (unresolved counts them: the save
// stays dirty so a later pass carries them once it has). Caller holds l.mu.
func (l *inboundLiveness) heldOriginsLocked(channel, watermark string) ([]livenessHeldOrigin, int) {
	prefix := channel + "|"
	now := l.now()
	var out []livenessHeldOrigin
	unresolved := 0
	for key, e := range l.origins {
		if e.source == inboundOriginUnknown || l.inFlight[key] || now.Sub(e.at) > livenessOriginTTL || !strings.HasPrefix(key, prefix) {
			continue
		}
		if e.source == inboundOriginLive && !l.liveDeliveredLocked(key, e) {
			if watermark == "" || compareSlackTS(key[len(prefix):], watermark) > 0 {
				unresolved++
			}
			continue
		}
		ts := key[len(prefix):]
		if watermark != "" && compareSlackTS(ts, watermark) <= 0 {
			continue
		}
		out = append(out, livenessHeldOrigin{TS: ts, Live: e.source == inboundOriginLive})
	}
	sort.Slice(out, func(i, j int) bool { return compareSlackTS(out[i].TS, out[j].TS) < 0 })
	return out, unresolved
}

// saveState persists the watermarks if anything changed. Errors are
// logged; nothing depends on the write succeeding.
func (l *inboundLiveness) saveState() {
	if l == nil || l.statePath == "" {
		return
	}
	l.mu.Lock()
	if !l.stateDirty {
		l.mu.Unlock()
		return
	}
	st := livenessState{LastInboundAt: l.lastInboundAt, Channels: make(map[string]*livenessChannel, len(l.channels))}
	unresolved := 0
	for id, ch := range l.channels {
		c := *ch
		// The pending set is the one reference-typed field: the encode below
		// runs outside the lock while the live path and the backfill shift
		// the same backing array (the lane's codex round 2).
		c.PendingTS = slices.Clone(ch.PendingTS)
		c.PendingThreads = slices.Clone(ch.PendingThreads)
		c.PendingMessages = maps.Clone(ch.PendingMessages)
		c.PendingHistory = slices.Clone(ch.PendingHistory)
		c.HeldOrigins = nil
		if floor, held := ch.originFloorLocked(); held {
			var n int
			c.HeldOrigins, n = l.heldOriginsLocked(id, floor)
			unresolved += n
		}
		st.Channels[id] = &c
	}
	// A live line still forwarding is not in this file; the dedup commit
	// that concludes it does not dirty the state, so the save stays dirty
	// until a pass finds every held line resolved (the lane's codex round
	// 8: a quiet channel's later saves, the shutdown save included, used to
	// skip the write and a restart replayed the delivered line).
	l.stateDirty = unresolved > 0
	l.mu.Unlock()
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		log.Printf("inbound liveness: encode state: %v", err)
		return
	}
	// The loader refuses a file above livenessStateMaxBytes, and a refused
	// file loses every watermark and hold: shed the held replays' texts
	// first (their dead letter then says to read the text from Slack), then
	// the held origins (a restart then replays delivered lines, a duplicate
	// over a loss), until it fits.
	for shed := 0; len(data) > livenessStateMaxBytes && shed < 2; shed++ {
		for _, ch := range st.Channels {
			switch shed {
			case 0:
				for ts, held := range ch.PendingMessages {
					held.Msg.Text = ""
					held.TextDropped = true
					ch.PendingMessages[ts] = held
				}
			case 1:
				ch.HeldOrigins = nil
			}
		}
		what := "the held replays' texts"
		if shed == 1 {
			what = "the held origins"
		}
		log.Printf("inbound liveness: state %s would be %d bytes (limit %d) — shedding %s", l.statePath, len(data), livenessStateMaxBytes, what)
		if data, err = json.MarshalIndent(st, "", "  "); err != nil {
			log.Printf("inbound liveness: encode state: %v", err)
			return
		}
	}
	if err := writeFile0600(l.statePath, data); err != nil {
		log.Printf("inbound liveness: write state %s: %v", l.statePath, err)
	}
}

// degradedReason reports the advisory service-health reason while a
// confirmed inbound stall is active, "" otherwise (gp-rol). Read by
// handleHealthz for the X-GC-Health header so `gc service list` shows
// the slack service as degraded — the exact signal the 2026-08-19
// outage lacked (state=ready for 26 minutes of dead inbound) — while
// gc keeps routing outbound /publish traffic.
func (l *inboundLiveness) degradedReason() string {
	if l == nil {
		return ""
	}
	l.mu.Lock()
	stalled := l.stalledSince
	last := l.lastInboundAt
	l.mu.Unlock()
	if stalled.IsZero() {
		return ""
	}
	reason := fmt.Sprintf("inbound_liveness stalled since %s (missed=%d)",
		stalled.UTC().Format(time.RFC3339), l.lastMissed.Load())
	if !last.IsZero() {
		reason += " last_inbound=" + last.UTC().Format(time.RFC3339)
	}
	return reason
}

// healthzDetail renders the liveness lines for /healthz. Nil receiver
// reports the watchdog as disabled.
func (l *inboundLiveness) healthzDetail() string {
	if l == nil {
		return "inbound_liveness=disabled\n"
	}
	l.mu.Lock()
	last := l.lastInboundAt
	stalled := l.stalledSince
	n := len(l.channels)
	l.mu.Unlock()
	state := "ok"
	if !stalled.IsZero() {
		state = "stalled"
	}
	if l.cfg.livenessStallAfter <= 0 {
		state = "watchdog_off"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "inbound_liveness=%s liveness_channels=%d liveness_probes=%d liveness_missed_total=%d liveness_backfilled_total=%d liveness_alarms=%d",
		state, n, l.probesTotal.Load(), l.missedTotal.Load(), l.backfilledTotal.Load(), l.alarmsTotal.Load())
	if !last.IsZero() {
		fmt.Fprintf(&b, " last_inbound=%s", last.UTC().Format(time.RFC3339))
	}
	if !stalled.IsZero() {
		fmt.Fprintf(&b, " stalled_since=%s", stalled.UTC().Format(time.RFC3339))
	}
	b.WriteString("\n")
	return b.String()
}

// slackHistoryClient is the production historyFetcher: bot-token reads
// against slackAPIBase.
type slackHistoryClient struct {
	token  string
	client *http.Client
}

func newSlackHistoryClient(token string) *slackHistoryClient {
	return &slackHistoryClient{token: token, client: &http.Client{Timeout: livenessFetchTimeout}}
}

type slackHistoryResp struct {
	OK       bool              `json:"ok"`
	Error    string            `json:"error,omitempty"`
	Messages []json.RawMessage `json:"messages,omitempty"`
	HasMore  bool              `json:"has_more,omitempty"`
}

func (c *slackHistoryClient) history(ctx context.Context, channel, oldest, latest string, limit int) ([]slackHistoryMessage, bool, error) {
	q := url.Values{}
	q.Set("channel", channel)
	q.Set("limit", strconv.Itoa(limit))
	if oldest != "" {
		q.Set("oldest", oldest)
	}
	if latest != "" {
		// The page below the last one read: Slack's latest is exclusive
		// (inclusive defaults to false), so the ts itself is not repeated.
		q.Set("latest", latest)
	}
	return c.get(ctx, "/conversations.history", q, "")
}

// fetchHistoryPage reads one history page under the per-read timeout.
func (l *inboundLiveness) fetchHistoryPage(ctx context.Context, channel, oldest, latest string, limit int) ([]slackHistoryMessage, bool, error) {
	fctx, cancel := context.WithTimeout(ctx, livenessFetchTimeout)
	defer cancel()
	return l.fetch.history(fctx, channel, oldest, latest, limit)
}

func (c *slackHistoryClient) replies(ctx context.Context, channel, threadTS, oldest string, limit int) ([]slackHistoryMessage, bool, error) {
	q := url.Values{}
	q.Set("channel", channel)
	q.Set("ts", threadTS)
	q.Set("limit", strconv.Itoa(limit))
	if oldest != "" {
		q.Set("oldest", oldest)
	}
	return c.get(ctx, "/conversations.replies", q, threadTS)
}

// get performs one read and returns messages oldest-first with Raw set.
// conversations.history returns newest-first; replies oldest-first —
// both are normalized here by ts.
func (c *slackHistoryClient) get(ctx context.Context, method string, q url.Values, dropTS string) ([]slackHistoryMessage, bool, error) {
	if c.token == "" {
		return nil, false, errors.New("slack token empty")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, slackAPIBase+method+"?"+q.Encode(), nil)
	if err != nil {
		return nil, false, fmt.Errorf("build %s: %w", method, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, false, fmt.Errorf("%s: %w", method, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, false, fmt.Errorf("read %s: %w", method, err)
	}
	if resp.StatusCode >= 300 {
		return nil, false, fmt.Errorf("%s HTTP %d: %s", method, resp.StatusCode, clipBodyForLog(body))
	}
	var hr slackHistoryResp
	if err := json.Unmarshal(body, &hr); err != nil {
		return nil, false, fmt.Errorf("decode %s: %w", method, err)
	}
	if !hr.OK {
		return nil, false, fmt.Errorf("%s not ok: %s", method, hr.Error)
	}
	out := make([]slackHistoryMessage, 0, len(hr.Messages))
	for _, raw := range hr.Messages {
		var m slackHistoryMessage
		if err := json.Unmarshal(raw, &m); err != nil || m.TS == "" {
			continue
		}
		if dropTS != "" && m.TS == dropTS {
			continue
		}
		m.Raw = append(json.RawMessage(nil), raw...)
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return compareSlackTS(out[i].TS, out[j].TS) < 0 })
	return out, hr.HasMore, nil
}

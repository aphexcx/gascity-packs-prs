package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
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
	// livenessOriginTTL / livenessOriginMax bound the origin seen-set.
	livenessOriginTTL = 3 * time.Hour
	livenessOriginMax = 16384
	// livenessMaxChannels caps the watched set (most recently active win).
	livenessMaxChannels = 50
	// livenessHistoryLimit is the per-call conversations.history page.
	livenessHistoryLimit = 200
	// livenessMaxThreadsPerChannel caps thread reads per channel per probe.
	livenessMaxThreadsPerChannel = 10
	// livenessThreadScanWindow is how far back parents are scanned for
	// fresh replies (history only returns parents by their own ts).
	livenessThreadScanWindow = 24 * time.Hour
	// livenessFetchTimeout bounds one Slack read.
	livenessFetchTimeout = 15 * time.Second
	// livenessMaxBackfillPerChannel caps replayed messages per channel
	// per pass so a long outage cannot flood sessions in one burst.
	livenessMaxBackfillPerChannel = 100
)

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
	// history returns messages in channel with ts > oldest (exclusive),
	// oldest-first, at most limit.
	history(ctx context.Context, channel, oldest string, limit int) ([]slackHistoryMessage, error)
	// replies returns the thread rooted at threadTS with ts > oldest,
	// oldest-first (the parent is omitted), at most limit.
	replies(ctx context.Context, channel, threadTS, oldest string, limit int) ([]slackHistoryMessage, error)
}

// livenessChannel is one watched channel's state.
type livenessChannel struct {
	TeamID      string    `json:"team_id,omitempty"`
	WatermarkTS string    `json:"watermark_ts"`
	LastSeenAt  time.Time `json:"last_seen_at"`
	Pinned      bool      `json:"pinned,omitempty"`
}

// livenessState is the persisted shape.
type livenessState struct {
	LastInboundAt time.Time                   `json:"last_inbound_at"`
	Channels      map[string]*livenessChannel `json:"channels"`
}

type originEntry struct {
	source inboundOriginSource
	at     time.Time
}

// missedMessage is one history message the adapter never received.
type missedMessage struct {
	channel string
	teamID  string
	msg     slackHistoryMessage
}

// inboundLiveness is the process-wide tracker. Safe for concurrent use.
type inboundLiveness struct {
	cfg   config
	fetch historyFetcher
	// deliver runs one synthesized envelope through the events handler
	// and returns the HTTP status it would have produced. Wired by
	// main(); nil means "alarm only" (tests and disabled backfill).
	deliver func(ctx context.Context, env slackEventEnvelope) int
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
	statePath     string
	stateDirty    bool
	botUserID     string
	apiAppID      string
	teamID        string
	probing       bool

	probesTotal     atomic.Uint64
	missedTotal     atomic.Uint64
	backfilledTotal atomic.Uint64
	alarmsTotal     atomic.Uint64
	lastMissed      atomic.Int64
}

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
	if ch.WatermarkTS == "" || compareSlackTS(ts, ch.WatermarkTS) > 0 {
		ch.WatermarkTS = ts
	}
	ch.LastSeenAt = now
	l.stateDirty = true
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
	l.mu.Unlock()
	if !due {
		return
	}
	l.probe(ctx, probeModeWatchdog, fmt.Sprintf("watchdog idle=%s", idle.Round(time.Second)))
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
	channels := l.snapshotChannelsLocked()
	l.mu.Unlock()
	defer func() {
		l.mu.Lock()
		l.probing = false
		l.mu.Unlock()
	}()
	l.probesTotal.Add(1)
	if len(channels) == 0 {
		log.Printf("inbound liveness: probe (%s) skipped — no watched channels yet (pin with SLACK_LIVENESS_CHANNELS)", reason)
		return
	}

	var missed []missedMessage
	var skipped []string
	for _, c := range channels {
		found, err := l.probeChannel(ctx, c.id, c.ch)
		if err != nil {
			skipped = append(skipped, c.id+":"+scrubSlackSecrets(err.Error()))
			continue
		}
		missed = append(missed, found...)
	}
	l.lastMissed.Store(int64(len(missed)))
	if len(skipped) > 0 {
		log.Printf("inbound liveness: probe (%s) skipped %d channel(s): %s", reason, len(skipped), strings.Join(skipped, "; "))
	}
	if len(missed) == 0 {
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
	delivered := l.backfill(ctx, missed)
	log.Printf("inbound liveness: backfill (%s) replayed %d/%d missed message(s)", reason, delivered, len(missed))
	if mode == probeModeReconnect && delivered > 0 && l.alert != nil {
		go l.postAlert(fmt.Sprintf(":arrows_counterclockwise: gc-slack-adapter backfilled %d message(s) that landed while inbound was down (%s; %s).",
			delivered, reason, summarizeByChannel(byChannel)))
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
		out = append(out, watchedChannel{id: id, ch: *ch})
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
func (l *inboundLiveness) probeChannel(ctx context.Context, channel string, ch livenessChannel) ([]missedMessage, error) {
	now := l.now()
	oldest := ch.WatermarkTS
	if win := l.cfg.backfillMaxWindow; win > 0 {
		floor := slackTSFromTime(now.Add(-win))
		if oldest == "" || compareSlackTS(oldest, floor) < 0 {
			oldest = floor
		}
	} else if oldest == "" {
		oldest = slackTSFromTime(now.Add(-l.cfg.livenessStallAfter - time.Minute))
	}
	// One read covers both questions. Parents newer than the watermark
	// are candidates themselves; parents from the last
	// livenessThreadScanWindow whose latest_reply moved past the
	// watermark point at threads with fresh replies (history only lists
	// parents by their own ts, so a reply posted during the gap into an
	// older thread is otherwise invisible). Newest-first paging means a
	// busy channel still yields the most recent livenessHistoryLimit.
	fetchFrom := oldest
	if scanOldest := slackTSFromTime(now.Add(-livenessThreadScanWindow)); compareSlackTS(scanOldest, oldest) < 0 {
		fetchFrom = scanOldest
	}
	fctx, cancel := context.WithTimeout(ctx, livenessFetchTimeout)
	defer cancel()
	all, err := l.fetch.history(fctx, channel, fetchFrom, livenessHistoryLimit)
	if err != nil {
		return nil, err
	}
	var missed []missedMessage
	var threads []slackHistoryMessage
	for _, m := range all {
		if m.ReplyCount > 0 && m.LatestReply != "" && compareSlackTS(m.LatestReply, oldest) > 0 {
			threads = append(threads, m)
		}
		if compareSlackTS(m.TS, oldest) <= 0 {
			continue
		}
		if l.originSource(channel, m.TS) != inboundOriginUnknown || !historyMessageDispatchable(m) {
			continue
		}
		missed = append(missed, missedMessage{channel: channel, teamID: ch.TeamID, msg: m})
	}
	// Most recently active threads first; cap reads per probe.
	sort.Slice(threads, func(i, j int) bool { return compareSlackTS(threads[i].LatestReply, threads[j].LatestReply) > 0 })
	if len(threads) > livenessMaxThreadsPerChannel {
		log.Printf("inbound liveness: channel=%s has %d threads with fresh replies; reading the %d most recent this probe", channel, len(threads), livenessMaxThreadsPerChannel)
		threads = threads[:livenessMaxThreadsPerChannel]
	}
	for _, p := range threads {
		rctx, rcancel := context.WithTimeout(ctx, livenessFetchTimeout)
		replies, err := l.fetch.replies(rctx, channel, p.TS, oldest, livenessHistoryLimit)
		rcancel()
		if err != nil {
			log.Printf("inbound liveness: thread read channel=%s ts=%s: %v", channel, p.TS, scrubSlackSecrets(err.Error()))
			continue
		}
		for _, m := range replies {
			if m.TS == p.TS || compareSlackTS(m.TS, oldest) <= 0 {
				continue
			}
			if l.originSource(channel, m.TS) != inboundOriginUnknown || !historyMessageDispatchable(m) {
				continue
			}
			missed = append(missed, missedMessage{channel: channel, teamID: ch.TeamID, msg: m})
		}
	}
	return missed, nil
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

// backfill replays missed messages, oldest first, through the events
// pipeline as synthesized envelopes. Each origin is marked backfilled
// BEFORE delivery so a racing live copy is dropped; a non-2xx outcome
// un-marks it so a later live/redelivered copy can still land. Returns
// the count delivered.
func (l *inboundLiveness) backfill(ctx context.Context, missed []missedMessage) int {
	perChannel := map[string]int{}
	delivered := 0
	for _, m := range missed {
		if ctx.Err() != nil {
			break
		}
		if perChannel[m.channel] >= livenessMaxBackfillPerChannel {
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
		l.mu.Unlock()
		status := l.deliver(ctx, env)
		if status < 200 || status >= 300 {
			l.mu.Lock()
			delete(l.origins, key)
			l.mu.Unlock()
			log.Printf("inbound liveness: backfill delivery channel=%s ts=%s status=%d — left for a live redelivery", m.channel, m.msg.TS, status)
			continue
		}
		delivered++
		l.backfilledTotal.Add(1)
		l.mu.Lock()
		l.touchChannelLocked(m.channel, m.teamID, m.msg.TS, now)
		l.mu.Unlock()
		log.Printf("inbound liveness: backfilled channel=%s ts=%s user=%s thread=%s text=%dch", m.channel, m.msg.TS, m.msg.User, m.msg.ThreadTS, len(m.msg.Text))
	}
	return delivered
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
// marshal the envelope, build the trusted "backfill" request, run the
// handler in-memory, return its status.
func deliverViaHandler(h http.Handler) func(ctx context.Context, env slackEventEnvelope) int {
	return func(ctx context.Context, env slackEventEnvelope) int {
		body, err := json.Marshal(env)
		if err != nil {
			return http.StatusInternalServerError
		}
		req, _ := http.NewRequestWithContext(withTrustedTransport(ctx, transportBackfill), http.MethodPost, "/slack/events", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.ContentLength = int64(len(body))
		req.RemoteAddr = transportBackfill
		w := newMemResponseWriter()
		h.ServeHTTP(w, req)
		return w.Status()
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
	if len(data) > 1<<20 {
		log.Printf("inbound liveness: state %s oversized (%d bytes) — starting fresh", l.statePath, len(data))
		return
	}
	var st livenessState
	if err := json.Unmarshal(data, &st); err != nil {
		log.Printf("inbound liveness: state %s corrupt (%v) — starting fresh", l.statePath, err)
		return
	}
	l.lastInboundAt = st.LastInboundAt
	for id, ch := range st.Channels {
		if id == "" || ch == nil {
			continue
		}
		ch.Pinned = false // re-derived from config each start
		l.channels[id] = ch
	}
	log.Printf("inbound liveness: loaded state %s (channels=%d last_inbound=%s)", l.statePath, len(l.channels), st.LastInboundAt.UTC().Format(time.RFC3339))
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
	for id, ch := range l.channels {
		c := *ch
		st.Channels[id] = &c
	}
	l.stateDirty = false
	l.mu.Unlock()
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		log.Printf("inbound liveness: encode state: %v", err)
		return
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
}

func (c *slackHistoryClient) history(ctx context.Context, channel, oldest string, limit int) ([]slackHistoryMessage, error) {
	q := url.Values{}
	q.Set("channel", channel)
	q.Set("limit", strconv.Itoa(limit))
	if oldest != "" {
		q.Set("oldest", oldest)
	}
	return c.get(ctx, "/conversations.history", q, "")
}

func (c *slackHistoryClient) replies(ctx context.Context, channel, threadTS, oldest string, limit int) ([]slackHistoryMessage, error) {
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
func (c *slackHistoryClient) get(ctx context.Context, method string, q url.Values, dropTS string) ([]slackHistoryMessage, error) {
	if c.token == "" {
		return nil, errors.New("slack token empty")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, slackAPIBase+method+"?"+q.Encode(), nil)
	if err != nil {
		return nil, fmt.Errorf("build %s: %w", method, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", method, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", method, err)
	}
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%s HTTP %d: %s", method, resp.StatusCode, clipBodyForLog(body))
	}
	var hr slackHistoryResp
	if err := json.Unmarshal(body, &hr); err != nil {
		return nil, fmt.Errorf("decode %s: %w", method, err)
	}
	if !hr.OK {
		return nil, fmt.Errorf("%s not ok: %s", method, hr.Error)
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
	return out, nil
}

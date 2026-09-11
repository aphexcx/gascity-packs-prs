package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Thread-context forwarding for cross-agent visibility on shared
// threads. Two beads compose into one mechanism:
//
//   gc-px8.5 — first-mention preamble: when an inbound carries
//     thread_ts and the targeted agent has not been seen on this
//     (target, channel, thread) before, prepend the prior-replies
//     window so the agent sees decision-making it joined mid-stream.
//
//   gc-px8.6 — cross-agent delta visibility: when the same target
//     is mentioned again on the same thread, prepend only the
//     replies posted since the target's last delivered context, so
//     mayor sees what PL replied between mayor's two mentions, and
//     vice versa, without redundant re-paste of context already
//     conveyed.
//
// The implementation is option B from the gc-px8.6 design: fetch
// conversations.replies on every inbound that carries thread_ts and
// is not the thread parent itself. The cache stores per-(target,
// channel, thread) the ts up-to-which preamble has been delivered;
// the formatter applies that as a lower bound so each agent's
// preamble is the delta of peer activity since its last visit.
// Trade-off: more API calls than gc-px8.5's single-shot policy; one
// fetch per inbound in a thread. Slack's tier-3 limit on
// conversations.replies (50/min) is comfortable for typical
// per-thread cadence.

// defaultThreadContextLimit caps how many thread replies the adapter
// asks Slack for when seeding context. Slack itself silently caps
// conversations.replies at 1000; we want a smaller window so a long-
// running thread doesn't dump a megabyte of history into a single
// bridge-mail body. 20 is generous for the priority-feature use case
// (a freshly-mentioned mayor seeing the recent decision-making) and
// is overrideable via SLACK_THREAD_CONTEXT_LIMIT.
const defaultThreadContextLimit = 20

// threadContextFetchTimeout bounds the conversations.replies HTTP
// round-trip. Slack's API typically responds in well under a second;
// 5s is comfortable headroom and keeps a stuck fetch from blocking
// the dispatch goroutine indefinitely while still holding the
// dispatchSem slot.
const threadContextFetchTimeout = 5 * time.Second

// threadContextCacheTTL bounds how long an idle (target, channel,
// thread) entry survives. Slack threads rarely stay active for a
// week; an evicted entry merely means the next mention on that
// thread re-delivers the full prior window instead of the delta — a
// one-time duplicate preamble, not context loss. Aligned with the
// INBOUND_FILE_TTL default (7d) so both per-thread artifacts age out
// on the same horizon.
const threadContextCacheTTL = 7 * 24 * time.Hour

// threadContextCacheMaxEntries hard-caps the cache so a pathological
// workload (many distinct threads inside one TTL window) cannot grow
// it without bound. When an insert pushes the map over the cap, the
// oldest-touched entry is evicted; the same benign full-preamble
// re-delivery applies.
const threadContextCacheMaxEntries = 4096

// threadContextCache tracks, per (target, channel, thread_ts) tuple,
// the ts of the most recent thread-context preamble the adapter has
// delivered for that target. The next inbound to the same target in
// the same thread uses that ts as a lower bound on which prior
// replies to include — peer activity newer than the last visit, not
// the entire history again.
//
// Entries are evicted after threadContextCacheTTL of inactivity
// (both reads and writes refresh an entry's clock, so live threads
// stay warm) and the map is capped at threadContextCacheMaxEntries
// with oldest-touched eviction. A target value of "" is a valid key
// for channel-bound inbounds without an explicit @handle.
//
// Errors during fetchThreadReplies do NOT advance the cached ts. A
// transient Slack 5xx or missing-scope 401 leaves the lower bound
// unchanged so the next inbound retries the fetch and (if it
// succeeds) still gets the priors that were missed during the error
// window. The trade-off is per-inbound logging on persistently-
// failing threads, which is the right operator signal — silent
// suppression of context loss is worse than a noisy log.
type threadContextCache struct {
	mu            sync.Mutex
	lastDelivered map[string]threadContextEntry
	// now is the clock; nil means time.Now. Injectable so tests can
	// drive TTL expiry without sleeping.
	now func() time.Time
}

// threadContextEntry pairs the delivered-up-to ts with the wall-clock
// instant the entry was last read or written, which drives eviction.
type threadContextEntry struct {
	ts      string
	touched time.Time
}

func newThreadContextCache() *threadContextCache {
	return &threadContextCache{lastDelivered: make(map[string]threadContextEntry)}
}

func (c *threadContextCache) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

// lastDeliveredFor returns the ts up-to-which the adapter has already
// delivered preamble context for the given (target, channel, thread)
// tuple. An empty return means "no preamble delivered yet" — the
// caller should treat all priors as new. Safe for concurrent
// callers. A nil receiver returns "" (no-op cache).
func (c *threadContextCache) lastDeliveredFor(target, channel, threadTS string) string {
	if c == nil {
		return ""
	}
	if channel == "" || threadTS == "" {
		return ""
	}
	key := threadCacheKey(target, channel, threadTS)
	now := c.clock()
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.lastDelivered[key]
	if !ok {
		return ""
	}
	if now.Sub(entry.touched) > threadContextCacheTTL {
		delete(c.lastDelivered, key)
		return ""
	}
	// Refresh the clock on read so a thread that is actively being
	// followed never ages out mid-conversation.
	entry.touched = now
	c.lastDelivered[key] = entry
	return entry.ts
}

// rollbackDelivered undoes one markDelivered whose delivery did not
// land (gp-32q, codex r4 P2 #5). The watermark is advanced BEFORE the
// forward, so a failed delivery would otherwise leave the next copy of
// that message — a same-ts twin's takeover, a Slack redelivery, the
// spool replay — reading the context as already conveyed and shipping
// a body with no preamble in front of it.
//
// prev is the value this delivery displaced ("" when there was none, in
// which case the entry is removed entirely).
//
// The rollback REGRESSES and never advances, and it runs even when a
// newer delivery has moved the watermark past advancedTo (codex r5 P2
// #3). Conditioning it on an exact match looked right and was not: with
// two failing deliveries A then B, A's rollback would no-op because the
// entry names B, and B's rollback would then put A back — leaving the
// cache permanently asserting that A's context arrived when neither
// delivery landed, which is the one direction this cache must never
// fail in. Regressing under a newer entry can instead re-send context
// that a SUCCESSFUL newer delivery already conveyed: one duplicated
// preamble, exactly what an eviction costs, and the trade this cache is
// designed around.
//
// An entry already older than advancedTo is left alone — some other
// failure regressed past this one, and its verdict is the conservative
// one.
func (c *threadContextCache) rollbackDelivered(target, channel, threadTS, advancedTo, prev string) {
	if c == nil {
		return
	}
	if channel == "" || threadTS == "" || advancedTo == "" {
		return
	}
	key := threadCacheKey(target, channel, threadTS)
	c.mu.Lock()
	defer c.mu.Unlock()
	cur, ok := c.lastDelivered[key]
	if !ok || cur.ts < advancedTo {
		return
	}
	if prev == "" {
		delete(c.lastDelivered, key)
		return
	}
	if prev >= cur.ts {
		// Never move the watermark forward on a failure.
		return
	}
	c.lastDelivered[key] = threadContextEntry{ts: prev, touched: c.clock()}
}

// markDelivered records ts as the high-water mark for which preamble
// context has been delivered for (target, channel, thread). Idempotent
// when called with a non-increasing ts: the stored value never
// regresses. Safe for concurrent callers. A nil receiver is a no-op.
func (c *threadContextCache) markDelivered(target, channel, threadTS, ts string) {
	if c == nil {
		return
	}
	if channel == "" || threadTS == "" || ts == "" {
		return
	}
	key := threadCacheKey(target, channel, threadTS)
	now := c.clock()
	c.mu.Lock()
	defer c.mu.Unlock()
	if prev, ok := c.lastDelivered[key]; ok && prev.ts >= ts {
		// Slack ts strings are lexically comparable in canonical
		// 17-char "<seconds>.<microseconds>" form; a regression here
		// would mean a stale handler raced ahead of a newer delivery,
		// which is a no-op for cache semantics — but the entry is
		// still live, so refresh its clock.
		prev.touched = now
		c.lastDelivered[key] = prev
		return
	}
	c.lastDelivered[key] = threadContextEntry{ts: ts, touched: now}
	if len(c.lastDelivered) > threadContextCacheMaxEntries {
		c.evictLocked(now)
	}
}

// evictLocked drops expired entries and, if the map is still over
// cap, the single oldest-touched entry. Called with c.mu held, only
// on the insert that pushed the map past the cap, so the O(n) scan
// amortizes to once per overflow insert.
func (c *threadContextCache) evictLocked(now time.Time) {
	var oldestKey string
	var oldestTouched time.Time
	for key, entry := range c.lastDelivered {
		if now.Sub(entry.touched) > threadContextCacheTTL {
			delete(c.lastDelivered, key)
			continue
		}
		if oldestKey == "" || entry.touched.Before(oldestTouched) {
			oldestKey, oldestTouched = key, entry.touched
		}
	}
	if len(c.lastDelivered) > threadContextCacheMaxEntries && oldestKey != "" {
		delete(c.lastDelivered, oldestKey)
	}
}

// threadCacheKey is the cache-map key shape for (target, channel,
// thread). Exposed for direct manipulation in tests; "|" is
// disallowed in Slack ids (channel, ts) and absent from typical
// handles, so the joined form is unambiguous in practice.
func threadCacheKey(target, channel, threadTS string) string {
	return target + "|" + channel + "|" + threadTS
}

// slackThreadMessage is the subset of the conversations.replies
// message shape the adapter consumes when building the preamble.
// Other fields are deliberately ignored to keep the JSON contract
// surface narrow.
type slackThreadMessage struct {
	User string `json:"user"`
	Text string `json:"text"`
	TS   string `json:"ts"`
	// ThreadTS identifies the thread the message belongs to (equal to
	// TS on a thread parent, absent on unthreaded messages). Consumed
	// by the reaction-target lookup (gp-by3) to thread the reaction
	// notification under the reacted-to message's thread.
	ThreadTS string `json:"thread_ts,omitempty"`
	// BotID is set when the message came from a bot rather than a
	// human user. The adapter's OWN posts are skipped from the
	// preamble — they are its outbound replies reflected back, already
	// in the session's transcript, and re-quoting them would only spend
	// budget (and invite feedback loops). PEER bot posts (a sister
	// city's mayor answering in the same thread) are quoted, labelled
	// with the bot's display name: they are the context that makes a
	// human's short reply legible (jg-ure5r8 — the 9/11 misread of
	// 「yes sling the fix bead」, which answered the peer, not us).
	BotID string `json:"bot_id,omitempty"`
	// Subtype, AppID, Username and BotProfile are the bot-provenance
	// fields conversations.replies carries for bot-authored messages;
	// the thread bot classifier reads them to tell self from peer and
	// to pick a label. A classic `bot_message` (chat.postMessage with a
	// username override — the shape a sister-city adapter posts) has
	// bot_id + app_id + username and NO user; an app-user post has
	// user + bot_id + app_id + bot_profile{name, app_id, user_id}.
	Subtype    string          `json:"subtype,omitempty"`
	AppID      string          `json:"app_id,omitempty"`
	Username   string          `json:"username,omitempty"`
	BotProfile json.RawMessage `json:"bot_profile,omitempty"`
}

// threadContextBotPostBytes caps each quoted PEER BOT post in the
// preamble. Agents write long reports (the 9/11 thread carried eight
// ~1 KB peer replies) while the channel reminder budget
// (defaultReminderTextBudget, 3500 bytes) sheds the preamble WHOLE when
// the unit is over budget — quoting peer posts in full would have
// replaced the entire context, the human's lines included, with the
// omission notice on exactly the thread this fix is for. The reader
// needs enough of each peer post to place the human's reply, not the
// report itself. Bytes, because the budget is bytes; the cut is
// rune-safe. Human posts stay unclipped (pre-existing behaviour).
const threadContextBotPostBytes = 280

// threadContextBotTotalBytes caps the bytes ALL quoted bot posts may
// take in one preamble. A per-post cap alone still lets a dozen peer
// reports push the unit over the reminder budget, where the composer
// sheds the preamble whole and the human lines that used to survive
// go with it (codex r2 P2). Bot quotes are allocated newest-first —
// the posts nearest the human's reply are the ones that place it —
// and the older overflow collapses into one count line. Human posts
// are never charged against this allowance. Sized so a full allowance
// plus a 20-line human window plus anchor and body still clears the
// default 3500-byte budget.
const threadContextBotTotalBytes = 1400

// threadBotAuthorFunc classifies a bot-authored thread reply for the
// preamble: include reports whether the post is quoted at all, label is
// the display name it is quoted under (rendered "@<label> (bot)"). A nil
// func drops every bot post — the pre-jg-ure5r8 behaviour, and the
// conservative direction when no self identity is known.
type threadBotAuthorFunc func(m slackThreadMessage) (label string, include bool)

// threadBotClassifier decides, for a bot-authored conversations.replies
// message, whether it is this adapter's own post (dropped) or a peer's
// (quoted, labelled). It mirrors the identity checklist of
// maybeDeliverPeerBotMessage (peer_bots.go, safety rule 1): a post is
// quoted only when at least one authoritative identity pair PROVES the
// author is not this adapter — the author's app id against a known self
// app id, or the author's bot user id against a known self bot user id.
// Wire fields (app_id / user / bot_profile) settle it without an API
// call for every real Slack post; a sparse message falls back to the
// per-bot-cached bots.info resolver. A post that cannot be proven
// not-self drops, with a log line as the operator signal — silently
// re-quoting our own reflected output is the failure the original
// blanket bot filter existed to prevent.
type threadBotClassifier struct {
	selfAppIDs  []string
	selfUserIDs []string
	// peers labels allowlisted peers with their configured label; nil is
	// fine (every peer then takes its wire / bots.info name).
	peers *peerBotsRegistry
	// authors is the bots.info resolver used only when the wire fields
	// cannot prove not-self; nil disables the fallback.
	authors companyAuthorResolver
	// resolved memoises resolver outcomes per bot id for the lifetime of
	// one classifier (one preamble): a thread of N sparse posts by one
	// stalling bot costs a single bots.info round-trip, not N — the
	// production resolver caches successes but not transient failures,
	// and this lookup runs synchronously while the inbound holds its
	// dispatch slot (codex r1 P2 #1).
	resolved map[string]threadBotResolution
}

// threadBotResolution is one memoised bots.info outcome.
type threadBotResolution struct {
	info    companyBotInfo
	outcome botResolveOutcome
}

// resolve returns the memoised bots.info outcome for botID, calling the
// resolver at most once per bot id per classifier.
func (c *threadBotClassifier) resolve(botID string) (companyBotInfo, botResolveOutcome) {
	if r, ok := c.resolved[botID]; ok {
		return r.info, r.outcome
	}
	info, outcome := c.authors.Resolve(botID)
	if c.resolved == nil {
		c.resolved = map[string]threadBotResolution{}
	}
	c.resolved[botID] = threadBotResolution{info: info, outcome: outcome}
	return info, outcome
}

// newThreadBotClassifier assembles the self identities the same way the
// peer-bot path does: SLACK_APP_ID and the envelope's api_app_id on the
// app side; the envelope authorizations' bot user and
// SLACK_SWITCHBOARD_BOT_USER_ID on the user side.
func newThreadBotClassifier(cfg config, env slackEventEnvelope) *threadBotClassifier {
	c := &threadBotClassifier{peers: cfg.peerBots, authors: cfg.peerAuthors}
	if cfg.slackAppID != "" {
		c.selfAppIDs = append(c.selfAppIDs, cfg.slackAppID)
	}
	if env.APIAppID != "" {
		c.selfAppIDs = append(c.selfAppIDs, env.APIAppID)
	}
	if own := env.botUserID(); own != "" {
		c.selfUserIDs = append(c.selfUserIDs, own)
	}
	if cfg.companySelfBotUserID != "" {
		c.selfUserIDs = append(c.selfUserIDs, cfg.companySelfBotUserID)
	}
	return c
}

// classify implements threadBotAuthorFunc. Only called for messages with
// a non-empty BotID.
func (c *threadBotClassifier) classify(m slackThreadMessage) (string, bool) {
	profile := parseBotProfile(m.BotProfile)
	appID := m.AppID
	if appID == "" {
		appID = profile.AppID
	}
	userID := m.User
	if userID == "" {
		userID = profile.UserID
	}
	self, proven := c.compareSelf(appID, userID)
	if self {
		return "", false
	}
	var info companyBotInfo
	if !proven && c.authors != nil && m.BotID != "" {
		if resolved, outcome := c.resolve(m.BotID); outcome == botResolveOK {
			info = resolved
			// The wire must not contradict the resolution (same
			// corroboration as the peer path); a contradiction is a
			// post we do not understand — drop.
			if (appID != "" && info.AppID != "" && appID != info.AppID) ||
				(userID != "" && info.UserID != "" && userID != info.UserID) {
				log.Printf("thread context: bot_id=%s ts=%s wire ids (app=%q user=%q) contradict bots.info (app=%q user=%q) — not quoted",
					m.BotID, m.TS, appID, userID, info.AppID, info.UserID)
				return "", false
			}
			if appID == "" {
				appID = info.AppID
			}
			if userID == "" {
				userID = info.UserID
			}
			self, proven = c.compareSelf(appID, userID)
			if self {
				return "", false
			}
		}
	}
	if !proven {
		log.Printf("thread context: cannot prove bot_id=%s ts=%s is not self (author app=%q user=%q; self ids known: apps=%d users=%d) — not quoted",
			m.BotID, m.TS, appID, userID, len(c.selfAppIDs), len(c.selfUserIDs))
		return "", false
	}
	return threadBotLabel(m, appID, userID, profile.Name, info.Name, c.peers), true
}

// compareSelf reports whether (appID, userID) names this adapter, and
// whether at least one pair was comparable at all (proven not-self when
// self is false).
func (c *threadBotClassifier) compareSelf(appID, userID string) (self, proven bool) {
	if appID != "" {
		for _, id := range c.selfAppIDs {
			if appID == id {
				return true, true
			}
		}
		if len(c.selfAppIDs) > 0 {
			proven = true
		}
	}
	if userID != "" {
		for _, id := range c.selfUserIDs {
			if userID == id {
				return true, true
			}
		}
		if len(c.selfUserIDs) > 0 {
			proven = true
		}
	}
	return false, proven
}

// threadBotLabel picks the display name a peer post is quoted under: the
// allowlist label when the peer is configured (peer_bots.json), else the
// bot_profile name, the classic bot_message username, the bots.info name,
// the bot user id, and the bot id as the last resort. Whitespace is
// collapsed so the label stays on the author line.
func threadBotLabel(m slackThreadMessage, appID, userID, profileName, resolvedName string, peers *peerBotsRegistry) string {
	if entry, ok := peers.matchPeer(m.BotID, appID); ok {
		return collapseLabel(entry.Label)
	}
	if entry, ok := peers.matchPeerByBotUserID(userID); ok {
		return collapseLabel(entry.Label)
	}
	for _, candidate := range []string{profileName, m.Username, resolvedName, userID, m.BotID} {
		if label := collapseLabel(candidate); label != "" {
			return label
		}
	}
	return "bot"
}

func collapseLabel(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// slackBotProfile is the subset of a message's bot_profile object the
// classifier reads.
type slackBotProfile struct {
	Name   string `json:"name"`
	AppID  string `json:"app_id"`
	UserID string `json:"user_id"`
}

func parseBotProfile(raw json.RawMessage) slackBotProfile {
	var bp slackBotProfile
	if len(raw) == 0 {
		return bp
	}
	if err := json.Unmarshal(raw, &bp); err != nil {
		return slackBotProfile{}
	}
	return bp
}

// slackConversationsRepliesResp is the top-level conversations.replies
// JSON response.
type slackConversationsRepliesResp struct {
	OK       bool                 `json:"ok"`
	Error    string               `json:"error,omitempty"`
	Messages []slackThreadMessage `json:"messages,omitempty"`
}

// fetchThreadReplies calls Slack's conversations.replies to retrieve
// messages in the thread rooted at threadTS. limit caps the response
// size; non-positive limits fall back to defaultThreadContextLimit.
//
// Returns the message slice exactly as Slack returned it (oldest-
// first by Slack's contract). The caller filters: drop the current
// message and any later replies before formatting.
func fetchThreadReplies(ctx context.Context, token, channel, threadTS string, limit int) ([]slackThreadMessage, error) {
	if token == "" {
		return nil, fmt.Errorf("slack token empty")
	}
	if channel == "" || threadTS == "" {
		return nil, fmt.Errorf("channel and thread_ts required")
	}
	if limit <= 0 {
		limit = defaultThreadContextLimit
	}
	q := url.Values{}
	q.Set("channel", channel)
	q.Set("ts", threadTS)
	q.Set("limit", strconv.Itoa(limit))

	reqURL := slackAPIBase + "/conversations.replies?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build conversations.replies request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("conversations.replies: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read conversations.replies body: %w", err)
	}
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("conversations.replies HTTP %d: %s", resp.StatusCode, clipBodyForLog(body))
	}
	var sr slackConversationsRepliesResp
	if err := json.Unmarshal(body, &sr); err != nil {
		return nil, fmt.Errorf("decode conversations.replies: %w (body=%s)", err, clipBodyForLog(body))
	}
	if !sr.OK {
		return nil, fmt.Errorf("conversations.replies not ok: %s", sr.Error)
	}
	return sr.Messages, nil
}

// formatThreadContextPreamble builds the bridge-mail preamble from
// messages in the half-open ts window (sinceTS, currentTS). Slack
// ts strings are lexically comparable when in the same canonical
// "<seconds>.<microseconds>" format.
//
// sinceTS == "" means "no lower bound" — include all priors;
// gc-px8.5's first-mention semantics. A non-empty sinceTS limits
// the preamble to peer activity newer than the target's last
// delivered context — gc-px8.6's cross-agent delta visibility.
//
// Whitespace-only messages are filtered. Bot-authored messages go
// through botAuthor: nil drops them all (pre-jg-ure5r8 behaviour);
// otherwise the adapter's own posts drop and peer bots quote under
// "@<label> (bot)". Returns "" when no messages survive filtering —
// caller MUST treat that as no-op so empty/short threads,
// current-message-only callbacks, and replays with no new peer
// activity carry no preamble overhead.
//
// resolveName maps a Slack user id to a display name for the author
// line (hq-uxln9); nil renders the raw id (pre-fix behavior, and what
// the table tests exercise).
//
// alreadyDelivered reports whether this audience already received the
// message as its own inbound (gp-729 item 2); such priors — the thread
// parent above all — collapse to a one-line count instead of a full
// re-quote. nil disables the filter (every prior quotes in full, the
// pre-gp-729 behavior). The conservative direction is always the full
// quote: an empty delivered-set only ever re-quotes, never loses.
func formatThreadContextPreamble(replies []slackThreadMessage, currentTS, sinceTS string, resolveName func(string) string, alreadyDelivered func(string) bool, botAuthor threadBotAuthorFunc) string {
	var prior []slackThreadMessage
	// botLabels carries the classifier's label per quoted bot post, keyed
	// by ts, so classification runs once per message.
	botLabels := map[string]string{}
	for _, m := range replies {
		if m.TS == "" {
			continue
		}
		if currentTS != "" && m.TS >= currentTS {
			continue
		}
		if sinceTS != "" && m.TS <= sinceTS {
			continue
		}
		if strings.TrimSpace(m.Text) == "" {
			continue
		}
		if m.BotID != "" {
			if botAuthor == nil {
				continue
			}
			label, include := botAuthor(m)
			if !include {
				continue
			}
			botLabels[m.TS] = label
		}
		prior = append(prior, m)
	}
	if len(prior) == 0 {
		return ""
	}
	var quoted []slackThreadMessage
	deliveredCount := 0
	deliveredNewest := ""
	for _, m := range prior {
		if alreadyDelivered != nil && alreadyDelivered(m.TS) {
			deliveredCount++
			if m.TS > deliveredNewest {
				deliveredNewest = m.TS
			}
			continue
		}
		quoted = append(quoted, m)
	}
	// Bot allowance: walk newest-first, keep the bot posts whose clipped
	// text fits threadContextBotTotalBytes, drop the rest from quoted
	// and count them. Human posts pass untouched.
	botOmitted := 0
	if len(quoted) > 0 {
		remaining := threadContextBotTotalBytes
		keep := make([]bool, len(quoted))
		for i := len(quoted) - 1; i >= 0; i-- {
			m := quoted[i]
			if m.BotID == "" {
				keep[i] = true
				continue
			}
			cost := len(clipBotPost(collapseThreadText(m.Text), threadContextBotPostBytes))
			if cost <= remaining {
				keep[i] = true
				remaining -= cost
				continue
			}
			botOmitted++
		}
		kept := quoted[:0]
		for i, m := range quoted {
			if keep[i] {
				kept = append(kept, m)
			}
		}
		quoted = kept
	}
	var b strings.Builder
	if deliveredCount > 0 {
		fmt.Fprintf(&b, "Thread context: %d earlier message", deliveredCount)
		if deliveredCount != 1 {
			b.WriteByte('s')
		}
		fmt.Fprintf(&b, " already delivered (newest ts %s) — not re-quoted.\n", deliveredNewest)
	}
	if len(quoted) == 0 && botOmitted == 0 {
		b.WriteString("\n---\n\n")
		return b.String()
	}
	fmt.Fprintf(&b, "Thread context (%d earlier message", len(quoted))
	if len(quoted) != 1 {
		b.WriteByte('s')
	}
	if botOmitted > 0 {
		fmt.Fprintf(&b, "; %d older peer-bot post", botOmitted)
		if botOmitted != 1 {
			b.WriteByte('s')
		}
		b.WriteString(" omitted for budget")
	}
	b.WriteString("):\n")
	for _, m := range quoted {
		author := m.User
		if m.BotID != "" {
			author = botLabels[m.TS] + " (bot)"
		} else if author != "" && resolveName != nil {
			author = resolveName(author)
		}
		if author == "" {
			author = "?"
		}
		text := collapseThreadText(m.Text)
		if m.BotID != "" {
			text = clipBotPost(text, threadContextBotPostBytes)
		}
		fmt.Fprintf(&b, "@%s: %s\n", author, text)
	}
	b.WriteString("\n---\n\n")
	return b.String()
}

// collapseThreadText collapses internal newlines to " | " so each prior
// message stays on a single line — the preamble is meant to be
// scannable, not a verbatim transcript reproduction.
func collapseThreadText(s string) string {
	return strings.ReplaceAll(strings.TrimSpace(s), "\n", " | ")
}

// clipBotPost returns s cut to at most maxBytes bytes (rune-safe) with
// an ellipsis when longer; a non-positive maxBytes disables clipping.
func clipBotPost(s string, maxBytes int) string {
	if maxBytes <= 0 || len(s) <= maxBytes {
		return s
	}
	return strings.TrimRight(clipRuneSafe(s, maxBytes), " |") + "…"
}

// clipBodyForLog truncates a Slack response body for inclusion in an
// error message. Slack error bodies are typically tiny; the cap is
// defensive against an unexpectedly large response.
func clipBodyForLog(body []byte) string {
	const maxLen = 256
	if len(body) <= maxLen {
		return string(body)
	}
	return string(body[:maxLen]) + "…"
}

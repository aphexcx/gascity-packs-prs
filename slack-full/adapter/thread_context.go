package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
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
// Trade-off: one bounded fetch per inbound in a thread. Page size is
// independent of the retained context window so long threads do not spend
// one API request for every 20 messages. Rate limits and timeouts return
// the context already fetched with an explicit incomplete-window notice.

// defaultThreadContextLimit caps the retained thread window (root plus
// newest replies). Cursor pages are consumed oldest-first; we want a smaller
// retained window so a long-
// running thread doesn't dump a megabyte of history into a single
// bridge-mail body. 20 is generous for the priority-feature use case
// (a freshly-mentioned mayor seeing the recent decision-making) and
// is overrideable via SLACK_THREAD_CONTEXT_LIMIT.
const defaultThreadContextLimit = 20

// Fetch at most four 500-message pages per inbound, independently of the
// retained window. An unfinished fetch preserves the newest replies in hand.
const threadContextPageSize = 500
const threadContextMaxPages = 4

// A 500-message page can legitimately exceed the old 1 MiB allowance.
// Keep the body bounded while allowing large ordinary text pages to decode.
const threadRepliesMaxResponseBytes = 32 << 20

const threadContextIncompleteNotice = "[thread context incomplete — showing fetched replies; read thread for the rest]\n\n"

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
	// BotID identifies a bot author for display provenance only.
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

// threadBotAuthorFunc supplies a display label. The legacy include result is
// ignored: history includes all authors, including when the classifier is nil.
type threadBotAuthorFunc func(m slackThreadMessage) (label string, include bool)

// threadBotClassifier supplies display provenance only. History is context,
// so own, peer and unknown bot messages all follow the human quote rule.
type threadBotClassifier struct {
	selfAppIDs  []string
	selfUserIDs []string
	// peers labels allowlisted peers with their configured label; nil is
	// fine (every peer then takes its wire / bots.info name).
	peers *peerBotsRegistry
	// authors is the bots.info resolver used only when the wire fields
	// lack an identity; nil disables the name lookup fallback.
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
	self, _ := c.compareSelf(appID, userID)
	var info companyBotInfo
	if appID == "" && userID == "" && c.authors != nil && m.BotID != "" {
		if resolved, outcome := c.resolve(m.BotID); outcome == botResolveOK {
			info = resolved
			appID, userID = info.AppID, info.UserID
			self, _ = c.compareSelf(appID, userID)
		}
	}
	label := threadBotLabel(m, appID, userID, profile.Name, info.Name, c.peers)
	provenance := "bot"
	if self {
		provenance = "own bot"
	} else if _, ok := c.peers.matchPeer(m.BotID, appID); ok {
		provenance = "peer bot"
	} else if _, ok := c.peers.matchPeerByBotUserID(userID); ok {
		provenance = "peer bot"
	}
	return label + " (" + provenance + ")", true
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
	OK               bool                 `json:"ok"`
	Error            string               `json:"error,omitempty"`
	Messages         []slackThreadMessage `json:"messages,omitempty"`
	HasMore          bool                 `json:"has_more,omitempty"`
	ResponseMetadata struct {
		NextCursor string `json:"next_cursor"`
	} `json:"response_metadata"`
}

// fetchThreadReplies follows conversations.replies cursors, retaining the
// root and newest limit-1 replies in chronological order. Optional beforeTS
// excludes the current and later messages before selecting the retained window.
// Non-positive limits fall back to defaultThreadContextLimit. A failed or
// incomplete page returns the retained window AND an error: callers must label
// the partial context and must not advance the delivered-context watermark.
func fetchThreadReplies(ctx context.Context, token, channel, threadTS string, limit int, beforeTS ...string) ([]slackThreadMessage, error) {
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
	q.Set("limit", strconv.Itoa(threadContextPageSize))
	if len(beforeTS) > 0 && beforeTS[0] != "" {
		q.Set("latest", beforeTS[0])
		q.Set("inclusive", "false")
	}
	// Slack pages oldest first. Keep the root for the anchor and the
	// newest limit-1 replies, using bounded memory while following cursors.
	var root *slackThreadMessage
	var recent []slackThreadMessage
	window := func() []slackThreadMessage {
		if root != nil {
			return append([]slackThreadMessage{*root}, recent...)
		}
		return recent
	}
	seenCursors := map[string]bool{}
	for page := 0; page < threadContextMaxPages; page++ {
		sr, err := fetchThreadRepliesPage(ctx, token, q)
		if err != nil {
			return window(), err
		}
		for i := range sr.Messages {
			sr.Messages[i].Text = redactInboundText(sr.Messages[i].Text)
			m := sr.Messages[i]
			if len(beforeTS) > 0 && beforeTS[0] != "" && m.TS >= beforeTS[0] {
				continue
			}
			if m.TS == threadTS {
				copy := m
				root = &copy
			} else {
				recent = append(recent, m)
			}
		}
		keep := limit
		if root != nil {
			keep--
		}
		if len(recent) > keep {
			recent = append([]slackThreadMessage(nil), recent[len(recent)-keep:]...)
		}
		cursor := strings.TrimSpace(sr.ResponseMetadata.NextCursor)
		if cursor == "" {
			if sr.HasMore {
				return window(), fmt.Errorf("conversations.replies has_more without cursor")
			}
			return window(), nil
		}
		if seenCursors[cursor] {
			return window(), fmt.Errorf("conversations.replies repeated cursor")
		}
		seenCursors[cursor] = true
		q.Set("cursor", cursor)
	}
	return window(), fmt.Errorf("conversations.replies exceeded %d-page context limit", threadContextMaxPages)
}

func fetchThreadRepliesPage(ctx context.Context, token string, q url.Values) (slackConversationsRepliesResp, error) {
	reqURL := slackAPIBase + "/conversations.replies?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return slackConversationsRepliesResp{}, fmt.Errorf("build conversations.replies request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return slackConversationsRepliesResp{}, fmt.Errorf("conversations.replies: %w", err)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, threadRepliesMaxResponseBytes))
	resp.Body.Close()
	if err != nil {
		return slackConversationsRepliesResp{}, fmt.Errorf("read conversations.replies body: %w", err)
	}
	if resp.StatusCode >= 300 {
		return slackConversationsRepliesResp{}, fmt.Errorf("conversations.replies HTTP %d: %s", resp.StatusCode, clipBodyForLog(body))
	}
	var sr slackConversationsRepliesResp
	if err := json.Unmarshal(body, &sr); err != nil {
		return slackConversationsRepliesResp{}, fmt.Errorf("decode conversations.replies: %w (body=%s)", err, clipBodyForLog(body))
	}
	if !sr.OK {
		return slackConversationsRepliesResp{}, fmt.Errorf("conversations.replies not ok: %s", sr.Error)
	}
	return sr, nil
}

// Ownership is an existence query over all pages, not a retained context
// window. Stop as soon as an own post is found, without losing early replies.
func fetchThreadHasOwnBotPost(ctx context.Context, token, channel, threadTS, botUserID, currentTS string) (bool, error) {
	q := url.Values{"channel": {channel}, "ts": {threadTS}, "limit": {strconv.Itoa(ownThreadScanLimit)}}
	if currentTS != "" {
		q.Set("latest", currentTS)
		q.Set("inclusive", "false")
	}
	seen := map[string]bool{}
	for {
		sr, err := fetchThreadRepliesPage(ctx, token, q)
		if err != nil {
			return false, err
		}
		if threadHasOwnBotPost(sr.Messages, botUserID, currentTS) {
			return true, nil
		}
		cursor := strings.TrimSpace(sr.ResponseMetadata.NextCursor)
		if cursor == "" {
			if sr.HasMore {
				return false, fmt.Errorf("conversations.replies has_more without cursor")
			}
			return false, nil
		}
		if seen[cursor] {
			return false, fmt.Errorf("conversations.replies repeated cursor")
		}
		seen[cursor] = true
		q.Set("cursor", cursor)
	}
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
// Whitespace-only messages are filtered. Bot classification only labels the
// author; no author class is filtered or assigned a separate quote budget.
// Returns "" when no messages survive the timestamp/content filters;
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
func formatThreadContextPreamble(replies []slackThreadMessage, threadTS, currentTS, sinceTS string, resolveName func(string) string, alreadyDelivered func(string) bool, botAuthor threadBotAuthorFunc, windows ...*injectionWindow) string {
	for _, window := range windows {
		window.SinceTS = sinceTS
	}
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
		isRoot := threadTS != "" && m.TS == threadTS
		if !isRoot && sinceTS != "" && m.TS <= sinceTS {
			for _, window := range windows {
				window.Skipped++
			}
			continue
		}
		if strings.TrimSpace(m.Text) == "" {
			continue
		}
		if m.BotID != "" || m.Subtype == "bot_message" {
			label := threadBotLabel(m, m.AppID, m.User, parseBotProfile(m.BotProfile).Name, "", nil) + " (bot)"
			if botAuthor != nil {
				if named, _ := botAuthor(m); named != "" {
					label = named
				}
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
	for _, m := range prior {
		if alreadyDelivered != nil && alreadyDelivered(m.TS) {
			deliveredCount++
			continue
		}
		quoted = append(quoted, m)
	}
	for _, window := range windows {
		window.Collapsed = deliveredCount
		if len(quoted) > 0 {
			window.Oldest, window.Newest = quoted[0].TS, quoted[len(quoted)-1].TS
		}
	}
	var b strings.Builder
	if deliveredCount > 0 {
		fmt.Fprintf(&b, "Thread context: %d earlier message", deliveredCount)
		if deliveredCount != 1 {
			b.WriteByte('s')
		}
		// The count alone (jg-fc1csv): the old "(newest ts …) — not
		// re-quoted" tail restated what "already delivered" says.
		b.WriteString(" already delivered.\n")
	}
	if len(quoted) == 0 {
		b.WriteString("\n---\n\n")
		return b.String()
	}
	fmt.Fprintf(&b, "Thread context (%d earlier message", len(quoted))
	if len(quoted) != 1 {
		b.WriteByte('s')
	}
	b.WriteString("):\n")
	for _, m := range quoted {
		author := m.User
		if label, ok := botLabels[m.TS]; ok {
			author = label
		} else if author != "" && resolveName != nil {
			author = resolveName(author)
		}
		if author == "" {
			author = "?"
		}
		text := collapseThreadText(m.Text)
		fmt.Fprintf(&b, "@%s: %s\n", author, text)
	}
	b.WriteString("\n---\n\n")
	return b.String()
}

// collapseThreadText collapses internal newlines to " | " so each prior
// message stays on a single line — the preamble is meant to be
// scannable, not a verbatim transcript reproduction.
func collapseThreadText(s string) string {
	s = redactInboundText(s)
	return strings.ReplaceAll(strings.TrimSpace(s), "\n", " | ")
}

// clipBodyForLog truncates a Slack response body for inclusion in an
// error message. Slack error bodies are typically tiny; the cap is
// defensive against an unexpectedly large response.
func clipBodyForLog(body []byte) string {
	body = []byte(redactInboundText(string(body)))
	const maxLen = 256
	if len(body) <= maxLen {
		return string(body)
	}
	return string(body[:maxLen]) + "…"
}

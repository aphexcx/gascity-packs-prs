package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// Allowlisted peers arrive as their own inbounds in every channel. Identity
// corroboration and self-echo protection govern admission; display formatting
// and context budgets use the same rules as human posts. Failed forwards alone
// enter the recovery FIFO. Registry wake/immediate_channels keys still parse
// for compatibility but no longer control delivery.

// peerBotEntry is one allowlisted fleet app in peer_bots.json. Label is the
// operator-facing peer name used in the provenance tag; at least one of the
// three identifiers must be set for the entry to be matchable.
type peerBotEntry struct {
	Label     string `json:"label"`
	AppID     string `json:"app_id,omitempty"`
	BotID     string `json:"bot_id,omitempty"`
	BotUserID string `json:"bot_user_id,omitempty"`
	// Deprecated: every allowlisted peer now wakes on arrival.
	Wake bool `json:"wake,omitempty"`
}

// peerBotsFile is the on-disk shape of peer_bots.json.
type peerBotsFile struct {
	Peers []peerBotEntry `json:"peers"`
	// Deprecated: every channel now delivers allowlisted peers immediately.
	ImmediateChannels []string `json:"immediate_channels,omitempty"`
}

// peerBotsRegistry is the read-mostly adapter-side view of peer_bots.json,
// operator-edited off-band and reloaded on SIGHUP via the same Stage/Commit
// pattern as the other CLI/operator-written registries.
type peerBotsRegistry struct {
	mu        sync.RWMutex
	peers     []peerBotEntry
	immediate map[string]bool
	diskPath  string
	dropLogs  peerBotDropLogs
}

// One diagnostic per bot per hour, including across registry reloads. Expired
// keys are removed as new diagnostics arrive; no message text is retained.
type peerBotDropLogs struct {
	mu   sync.Mutex
	last map[string]time.Time
}

func (l *peerBotDropLogs) allow(botID string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if last, ok := l.last[botID]; ok && now.Sub(last) < time.Hour {
		return false
	}
	for id, last := range l.last {
		if now.Sub(last) >= time.Hour {
			delete(l.last, id)
		}
	}
	if l.last == nil {
		l.last = make(map[string]time.Time)
	}
	l.last[botID] = now
	return true
}

var missingPeerRegistryDropLogs peerBotDropLogs

func (r *peerBotsRegistry) logDroppedBot(reason string, msg slackMessageEvent, botID, appID, botUserID string) {
	limiter := &missingPeerRegistryDropLogs
	if r != nil {
		limiter = &r.dropLogs
	}
	if limiter.allow(botID, time.Now()) {
		log.Printf("peer-bot: %s chan=%q ts=%q bot_id=%q app_id=%q bot_user_id=%q — dropped",
			reason, msg.Channel, msg.TS, botID, appID, botUserID)
	}
}

// peerBotsSnapshot is a parsed-but-not-yet-committed view of peer_bots.json.
// nil = "file absent" sentinel — same SIGHUP semantics as the siblings.
type peerBotsSnapshot struct {
	peers     []peerBotEntry
	immediate map[string]bool
}

// maxPeerBotsBytes caps the JSON file size; a fleet allowlist is a handful
// of entries, so 1 MiB is orders of magnitude above any healthy install.
const maxPeerBotsBytes = 1 << 20

// newPeerBotsRegistry opens (or treats-as-empty) the registry at diskPath.
// A missing file yields an empty allowlist — peer visibility simply stays
// off and every bot-authored message keeps the legacy drop behavior.
func newPeerBotsRegistry(diskPath string) (*peerBotsRegistry, error) {
	r := &peerBotsRegistry{diskPath: diskPath, immediate: map[string]bool{}}
	snap, err := parsePeerBots(diskPath)
	if err != nil {
		return nil, fmt.Errorf("load peer bots from %s: %w", diskPath, err)
	}
	if snap != nil {
		r.peers, r.immediate = snap.peers, snap.immediate
	}
	return r, nil
}

// parsePeerBots reads diskPath into a ready-to-commit snapshot. nil + nil =
// "file absent" sentinel for SIGHUP semantics. Entries missing a label or
// carrying no identifier at all are rejected at parse time so a corrupt
// write can't silently widen or blank the allowlist.
func parsePeerBots(diskPath string) (*peerBotsSnapshot, error) {
	if diskPath == "" {
		return nil, nil
	}
	f, err := openRegistryFile(diskPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxPeerBotsBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", diskPath, err)
	}
	if int64(len(data)) > maxPeerBotsBytes {
		return nil, fmt.Errorf("peer bots file %s exceeds %d bytes", diskPath, maxPeerBotsBytes)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var stored peerBotsFile
	if err := dec.Decode(&stored); err != nil {
		return nil, fmt.Errorf("decode peer bots: %w", err)
	}
	for i, p := range stored.Peers {
		if strings.TrimSpace(p.Label) == "" {
			return nil, fmt.Errorf("peer bots: entry %d has empty label", i)
		}
		if p.AppID == "" && p.BotID == "" && p.BotUserID == "" {
			return nil, fmt.Errorf("peer bots: entry %d (%q) has no app_id, bot_id, or bot_user_id", i, p.Label)
		}
	}
	immediate := make(map[string]bool, len(stored.ImmediateChannels))
	for i, ch := range stored.ImmediateChannels {
		if strings.TrimSpace(ch) == "" {
			return nil, fmt.Errorf("peer bots: immediate_channels entry %d is empty", i)
		}
		immediate[ch] = true
	}
	return &peerBotsSnapshot{peers: stored.Peers, immediate: immediate}, nil
}

// Stage parses the on-disk file into a snapshot ready for atomic Commit.
// nil snapshot + nil error = file absent, preserve live state.
func (r *peerBotsRegistry) Stage() (*peerBotsSnapshot, error) {
	return parsePeerBots(r.diskPath)
}

// Commit atomically swaps the in-memory snapshot under the write lock.
func (r *peerBotsRegistry) Commit(snap *peerBotsSnapshot) {
	if snap == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.peers, r.immediate = snap.peers, snap.immediate
}

// Len returns the number of allowlisted peers currently loaded, for the
// startup / SIGHUP log lines. Nil-safe.
func (r *peerBotsRegistry) Len() int {
	if r == nil {
		return 0
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.peers)
}

// matchPeer returns the first allowlist entry matching the author's
// event-declared identifiers (bot_id, or the author app id from the event /
// bot_profile). An entry that only names a bot_user_id matches later,
// against the bots.info resolution — see deliverPeerBotMessage. Nil-safe.
func (r *peerBotsRegistry) matchPeer(botID, authorAppID string) (peerBotEntry, bool) {
	if r == nil {
		return peerBotEntry{}, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, p := range r.peers {
		if p.BotID != "" && p.BotID == botID {
			return p, true
		}
		if p.AppID != "" && authorAppID != "" && p.AppID == authorAppID {
			return p, true
		}
	}
	return peerBotEntry{}, false
}

// matchPeerByBotUserID returns the first entry whose bot_user_id equals the
// resolved author user id — the fallback for entries configured with only a
// bot_user_id (no bot_id/app_id the event could match directly). Nil-safe.
func (r *peerBotsRegistry) matchPeerByBotUserID(botUserID string) (peerBotEntry, bool) {
	if r == nil || botUserID == "" {
		return peerBotEntry{}, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, p := range r.peers {
		if p.BotUserID != "" && p.BotUserID == botUserID {
			return p, true
		}
	}
	return peerBotEntry{}, false
}

// immediateChannel exposes the deprecated config value for compatibility
// tests. Delivery no longer consults it: every channel is immediate.
func (r *peerBotsRegistry) immediateChannel(channel string) bool {
	if r == nil {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.immediate[channel]
}

// peerContextItem is one failed forward awaiting recovery. Text is whole;
// this queue has no ambient traffic, item clip or silent eviction.
type peerContextItem struct {
	Label, ActorID, Channel, TS, ThreadTS, Text, Files string
	// Keep the original envelope's attachment capabilities and provenance
	// through recovery and any later held-delivery dead letter.
	inbound externalInboundMessage
}

type peerContextBuffer struct {
	mu      sync.Mutex
	pending map[string][]peerContextItem
}

func newPeerContextBuffer() *peerContextBuffer {
	return &peerContextBuffer{pending: make(map[string][]peerContextItem)}
}

func (b *peerContextBuffer) add(item peerContextItem) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, queued := range b.pending[item.Channel] {
		if queued.TS == item.TS {
			return
		}
	}
	b.pending[item.Channel] = append(b.pending[item.Channel], item)
}

func (b *peerContextBuffer) flush(channel string) ([]peerContextItem, int) {
	if b == nil {
		return nil, 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	items := b.pending[channel]
	delete(b.pending, channel)
	return items, 0
}

// Restore precedes concurrent arrivals; same-ts redeliveries cannot duplicate
// the queue. The old dropped parameter is always zero in new deliveries.
func (b *peerContextBuffer) restore(channel string, items []peerContextItem, _ int) {
	if b == nil || len(items) == 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	combined := append(append([]peerContextItem(nil), items...), b.pending[channel]...)
	seen := map[string]bool{}
	kept := combined[:0]
	for _, item := range combined {
		if !seen[item.TS] {
			kept = append(kept, item)
			seen[item.TS] = true
		}
	}
	b.pending[channel] = kept
}

func (b *peerContextBuffer) contains(channel, ts string) bool {
	if b == nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, item := range b.pending[channel] {
		if item.TS == ts {
			return true
		}
	}
	return false
}

func (b *peerContextBuffer) count(channel string) int {
	if b == nil {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.pending[channel])
}

func formatPeerContextLine(item peerContextItem) string {
	msg := externalInboundMessage{
		Conversation:      conversationRef{ConversationID: item.Channel},
		Actor:             externalActor{ID: item.ActorID, DisplayName: item.Label, IsBot: true},
		ProviderMessageID: item.TS, ReplyToMessageID: item.ThreadTS,
	}
	anchor := ""
	if item.ThreadTS != "" && item.ThreadTS != item.TS {
		anchor = formatThreadReplyAnchor(item.ThreadTS, "", "")
	}
	return minimalReminder(config{}, msg, item.Text, item.Files, anchor)
}

func formatPeerContextBlock(items []peerContextItem, _ int) string {
	lines := make([]string, 0, len(items))
	for _, item := range items {
		lines = append(lines, formatPeerContextLine(item))
	}
	return strings.Join(lines, "\n\n")
}

func recordRecoveredPeers(cfg config, items []peerContextItem) {
	for _, item := range items {
		cfg.deliveredIDs.record("", item.Channel, item.TS)
		cfg.channelClaims.commit(channelDeliveryClaimKey(item.Channel, item.TS))
	}
}

func attachRecoveredPeers(msg *externalInboundMessage, items []peerContextItem) {
	if len(items) == 0 {
		return
	}
	msg.Attachments = append([]externalAttachment(nil), msg.Attachments...)
	msg.trustedFilePaths = append([]string(nil), msg.trustedFilePaths...)
	for _, item := range items {
		msg.Attachments = append(msg.Attachments, item.inbound.Attachments...)
		msg.trustedFilePaths = append(msg.trustedFilePaths, item.inbound.trustedFilePaths...)
	}
}

func (item peerContextItem) withholdRejectedAttachments(cause error) peerContextItem {
	if permanentDeliveryFailure(cause) && len(item.inbound.Attachments) > 0 {
		p := withholdAttachments(pendingChannelInbound{inbound: item.inbound, body: item.Text, files: item.Files}, cause)
		item.inbound, item.Files = p.inbound, p.files
	}
	return item
}

// postWithPeerRecovery keeps a refused rider from charging an unrelated
// successor's rejection ladder. A payload refusal returns riders to the FIFO
// and retries the new message alone, visibly counting every deferred entry.
func postWithPeerRecovery(cfg config, base externalInboundMessage, items []peerContextItem) (externalInboundMessage, []peerContextItem, deliveryReceipt, error) {
	post := func(msg externalInboundMessage) (deliveryReceipt, error) {
		r, err := postInboundWithReceipt(cfg, msg)
		for attempt := 0; err == nil && r.verdict(cfg.deliveryReceiptGate) == receiptUnconfirmed && attempt < deliveryReceiptRepostAttempts && receiptRepostAllowed(cfg); attempt++ {
			log.Printf("inbound: chan=%s ts=%s gc did not vouch for delivery — re-posting in place (attempt %d/%d)",
				msg.Conversation.ConversationID, msg.ProviderMessageID, attempt+1, deliveryReceiptRepostAttempts)
			r, err = postInboundWithReceipt(cfg, msg)
		}
		return r, err
	}
	channel := base.Conversation.ConversationID
	env := base
	env.Text, items = peerRecovery(cfg, channel, base.Text, items)
	attachRecoveredPeers(&env, items)
	receipt, err := post(env)
	if !permanentDeliveryFailure(err) || len(items) == 0 {
		return env, items, receipt, err
	}
	for i := range items {
		items[i] = items[i].withholdRejectedAttachments(err)
	}
	cfg.peerContext.restore(channel, items, 0)
	env = base
	env.Text += fmt.Sprintf("\n[%d omitted — awaiting delivery]", cfg.peerContext.count(channel))
	receipt, err = post(env)
	return env, nil, receipt, err
}

// peerAuthorIdentifiers extracts the author-declared identifiers of a
// bot-authored message event: the bot id and the author app id (event-level
// app_id, falling back to bot_profile.app_id).
func peerAuthorIdentifiers(msg slackMessageEvent) (botID, authorAppID string) {
	authorAppID = msg.AppID
	if authorAppID == "" {
		authorAppID = parseBotProfileAppID(msg.BotProfile)
	}
	return msg.BotID, authorAppID
}

// parseBotProfileName extracts bot_profile.name from a raw bot_profile
// object, tolerating absence — display-only input for unknownBotLabel.
func parseBotProfileName(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var bp struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &bp); err != nil {
		return ""
	}
	return bp.Name
}

// unknownBotLabel picks the provenance label for a bot with no allowlist
// entry (gp-9e7 item 3): the event's own bot_profile name, the bots.info
// name, then the resolved bot user id / bot id as last resorts. Display
// only — identity checks ran against the resolution before this point.
func unknownBotLabel(msg slackMessageEvent, botID string, info companyBotInfo) string {
	if name := strings.TrimSpace(parseBotProfileName(msg.BotProfile)); name != "" {
		return name
	}
	if name := strings.TrimSpace(info.Name); name != "" {
		return name
	}
	if info.UserID != "" {
		return info.UserID
	}
	return botID
}

// maybeDeliverPeerBotMessage admits an allowlisted non-self author, then
// uses the ordinary single-message composer, delivery receipt and dedup path.
// A bot's text does not grant aliases or change channel ownership.
func maybeDeliverPeerBotMessage(cfg config, env slackEventEnvelope, msg slackMessageEvent) {
	// Only plain "message" events. A peer post @-mentioning our bot ALSO
	// fires an app_mention delivery for the same ts (hw-vzd5y edge case 2);
	// admitting both would double-deliver the post since the two carry
	// distinct event_ids that dedup cannot collapse.
	if msg.Type != "message" || msg.Channel == "" || msg.TS == "" {
		return
	}
	if strings.TrimSpace(msg.Text) == "" && len(msg.Files) == 0 {
		return
	}

	botID, authorAppID := peerAuthorIdentifiers(msg)

	// Self-guard, declared ids: our own posts must never come back to us
	// (safety rule 1), even when the operator misconfigures the allowlist
	// with our own app.
	if cfg.slackAppID != "" && authorAppID == cfg.slackAppID {
		return
	}

	entry, matched := cfg.peerBots.matchPeer(botID, authorAppID)

	// Every inbound requires a definitive bots.info resolution of the author's
	// bot user id and can prove it is not our own. Reuses the company
	// gateway's resolver scaffolding (company_authors.go); the per-bot
	// TTL cache keeps this to one API call per bot, not per post. Without
	// a bot_id there is nothing to resolve against — drop.
	if cfg.peerAuthors == nil || botID == "" {
		return
	}
	info, outcome := cfg.peerAuthors.Resolve(botID)
	if outcome != botResolveOK {
		if outcome == botResolveTransient {
			log.Printf("peer-bot: bots.info transient failure for bot_id=%s chan=%s ts=%s — bot post dropped (context is best-effort)",
				botID, msg.Channel, msg.TS)
		}
		return
	}
	// Corroborate event-declared identifiers against the resolution, same
	// checklist as resolveCompanyAuthor: a mismatch fails closed.
	if msg.AppID != "" && msg.AppID != info.AppID {
		cfg.peerBots.logDroppedBot("contradicted", msg, botID, msg.AppID, info.UserID)
		return
	}
	if bp := parseBotProfileAppID(msg.BotProfile); bp != "" && bp != info.AppID {
		cfg.peerBots.logDroppedBot("contradicted", msg, botID, bp, info.UserID)
		return
	}
	// Human corroboration (fix round 3b), completing the
	// resolveCompanyAuthor checklist: a bot-shaped event can carry a
	// `user` field, and when it names anyone the resolution does not
	// corroborate — a different user id, or no resolved user id at all
	// (bots.info may legitimately omit user_id) — this may be a HUMAN
	// message dressed as a bot. A human must never be buffered or woken
	// as a bot: drop.
	if msg.User != "" && msg.User != info.UserID {
		cfg.peerBots.logDroppedBot("contradicted", msg, botID, info.AppID, msg.User)
		return
	}

	// Self-guard, resolved ids (safety rule 1) — fail CLOSED (fix round
	// 3a): delivery requires at least one authoritative identity pair
	// that PROVES the author is not this adapter itself, with the
	// bots.info resolution on the author side and a known self identity
	// on the other. Self app ids: SLACK_APP_ID and the envelope's
	// api_app_id (the app this event was delivered to — present on every
	// real Events API / Socket Mode delivery). Self user ids: the
	// envelope authorizations' bot user and SLACK_SWITCHBOARD_BOT_USER_ID.
	// bots.info can succeed without a user_id, and a deployment can lack
	// every self identity — in either case, when NO pair is comparable,
	// the post drops rather than passing: an own post slipping through an
	// allowlist wake grant would reach postInbound as a self-wake loop.
	selfAppIDs := make([]string, 0, 2)
	if cfg.slackAppID != "" {
		selfAppIDs = append(selfAppIDs, cfg.slackAppID)
	}
	if env.APIAppID != "" {
		selfAppIDs = append(selfAppIDs, env.APIAppID)
	}
	selfUserIDs := make([]string, 0, 2)
	if own := env.botUserID(); own != "" {
		selfUserIDs = append(selfUserIDs, own)
	}
	if cfg.companySelfBotUserID != "" {
		selfUserIDs = append(selfUserIDs, cfg.companySelfBotUserID)
	}
	provenNotSelf := false
	if info.AppID != "" {
		for _, id := range selfAppIDs {
			if info.AppID == id {
				return // resolved author IS this adapter's app
			}
		}
		if len(selfAppIDs) > 0 {
			provenNotSelf = true
		}
	}
	if info.UserID != "" {
		for _, id := range selfUserIDs {
			if info.UserID == id {
				return // resolved author IS this adapter's bot user
			}
		}
		if len(selfUserIDs) > 0 {
			provenNotSelf = true
		}
	}
	if !provenNotSelf {
		log.Printf("peer-bot: cannot prove bot_id=%s is not self (resolved app=%q user=%q; self ids known: apps=%d users=%d) chan=%s ts=%s — dropped fail-closed",
			botID, info.AppID, info.UserID, len(selfAppIDs), len(selfUserIDs), msg.Channel, msg.TS)
		return
	}

	if !matched {
		// Re-match with the RESOLVED app id (gp-bhq D): an app_id-only
		// entry can never match a sparse event — one carrying neither
		// app_id nor bot_profile.app_id on the wire — but bots.info just
		// resolved the author's real app id, and the corroboration
		// checks above proved the event contradicts nothing about it.
		// Without this the peer silently degrades to an unknown bot,
		// losing its label and its admission to the inbound path.
		// Deliberately AFTER the fail-closed self-guard: an author that
		// cannot be proven not-self never reaches any match.
		entry, matched = cfg.peerBots.matchPeer(botID, info.AppID)
	}
	if !matched {
		entry, matched = cfg.peerBots.matchPeerByBotUserID(info.UserID)
	}
	if matched {
		// Config corroboration: an entry that pins ids the resolution
		// contradicts does not match (fail closed) — and a contradicted
		// author must not slip through as a generic unknown bot either,
		// so this stays a hard drop.
		if (entry.BotID != "" && entry.BotID != botID) ||
			(entry.BotUserID != "" && entry.BotUserID != info.UserID) ||
			(entry.AppID != "" && info.AppID != "" && entry.AppID != info.AppID) {
			cfg.peerBots.logDroppedBot("contradicted", msg, botID, info.AppID, info.UserID)
			return
		}
	} else {
		cfg.peerBots.logDroppedBot("unlisted", msg, botID, info.AppID, info.UserID)
		return // Only the registry allowlist can originate an inbound.
	}

	actorID := info.UserID
	if actorID == "" {
		actorID = botID
	}
	body := rewriteSlackUserMentions(cfg, msg.Text)
	attachments := downloadSlackFiles(cfg, msg.Channel, msg.TS, msg.Files)
	files := formatInboundFilesBlock(msg.Files, attachments)
	inbound := externalInboundMessage{
		ProviderMessageID: msg.TS,
		Conversation: conversationRef{ScopeID: cfg.cityName, Provider: cfg.provider, AccountID: cfg.accountID,
			ConversationID: msg.Channel, Kind: slackKindFromChannelType(msg.ChannelType, msg.Channel)},
		Actor: externalActor{ID: actorID, DisplayName: entry.Label, IsBot: true},
		Text:  body, ReplyToMessageID: msg.ThreadTS, Attachments: attachments,
		DedupKey: "slack-" + msg.TS, ReceivedAt: time.Now().UTC(),
	}
	item := peerContextItem{Label: entry.Label, ActorID: actorID, Channel: msg.Channel,
		TS: msg.TS, ThreadTS: msg.ThreadTS, Text: body, Files: files, inbound: inbound}
	// Like an urgent human arrival, deliver older buffered messages first.
	// No twin is withheld: a queued same-ts copy can settle normally, and
	// the delivered-ID check below then suppresses this duplicate.
	cfg.coalescer.flushAheadOf(msg.Channel, "")
	release := cfg.coalescer.holdDelivery(msg.Channel)
	defer release()
	if cfg.deliveredIDs.seen("", msg.Channel, msg.TS) {
		return
	}
	// A queued copy owns recovery. A duplicate Slack event must not send it
	// both as the new message and as a rider in the same reminder.
	if cfg.peerContext.contains(msg.Channel, msg.TS) {
		return
	}
	pending := pendingChannelInbound{inbound: inbound, body: body, files: files}
	complete := false
	if msg.ThreadTS != "" && msg.ThreadTS != msg.TS {
		author, line := cfg.injectionParents.get(msg.Channel, msg.ThreadTS)
		if !cfg.minimalInjection() && cfg.threadContextCache != nil {
			ctx, cancel := context.WithTimeout(context.Background(), threadContextFetchTimeout)
			replies, err := fetchThreadReplies(ctx, cfg.slackBotToken, msg.Channel, msg.ThreadTS, cfg.slackThreadContextLimit, msg.TS)
			cancel()
			window := injectionWindow{MessageTS: msg.TS, ThreadTS: msg.ThreadTS, Known: true}
			classifier := newThreadBotClassifier(cfg, env).classify
			for _, m := range replies {
				if m.TS == msg.ThreadTS {
					author = resolveUserDisplayName(cfg, m.User)
					if m.BotID != "" {
						author, _ = classifier(m)
					}
					line = rewriteSlackUserMentions(cfg, m.Text)
					break
				}
			}
			already := func(ts string) bool {
				// Flush-ahead can fail or defer under backoff. A still-pending
				// message is not evidence of delivery and must remain quoted.
				return cfg.deliveredIDs.seen("", msg.Channel, ts)
			}
			pending.preamble = formatThreadContextPreamble(replies, msg.ThreadTS, msg.TS,
				cfg.threadContextCache.lastDeliveredFor("", msg.Channel, msg.ThreadTS),
				func(id string) string { return resolveUserDisplayName(cfg, id) }, already, classifier, &window)
			if err != nil {
				pending.preamble = threadContextIncompleteNotice + pending.preamble
			} else {
				complete = true
			}
			pending.preamble = rewriteSlackUserMentions(cfg, pending.preamble)
			pending.inbound.injectionWindows = []injectionWindow{window}
		}
		pending.threadAnchor = formatThreadReplyAnchor(msg.ThreadTS, author, line)
	} else if cfg.minimalInjection() {
		cfg.injectionParents.remember(msg.Channel, msg.TS, entry.Label, body)
	}
	if err := deliverCoalescedBatch(cfg, msg.Channel, []pendingChannelInbound{pending}); err != nil {
		cfg.peerContext.add(item.withholdRejectedAttachments(err))
		log.Printf("peer-bot: immediate forward failed chan=%s ts=%s — queued for recovery", msg.Channel, msg.TS)
		return
	}
	if complete {
		cfg.threadContextCache.markDelivered("", msg.Channel, msg.ThreadTS, msg.TS)
	}
	log.Printf("peer-bot: delivered immediate post chan=%s ts=%s label=%q", msg.Channel, msg.TS, entry.Label)

}

// sortedPeerLabels returns the loaded allowlist labels, sorted, for tests
// and log lines.
func (r *peerBotsRegistry) sortedPeerLabels() []string {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.peers))
	for _, p := range r.peers {
		out = append(out, p.Label)
	}
	sort.Strings(out)
	return out
}

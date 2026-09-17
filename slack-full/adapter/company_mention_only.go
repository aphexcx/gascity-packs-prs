package main

import (
	"context"
	"encoding/json"
	"log"
	"sort"
	"strings"
	"time"
)

// Mention-only lane for imported company rooms (jg-vobf70 round 5).
//
// handleSlackEvents hands every message in an imported company room to
// the company gateway, which owns the HTTP response and delivers to the
// room's COMPANY members (company_bindings). The legacy dispatcher — and
// with it the mention-only block in processSlackEvent — never sees the
// event. A session bound to such a room with `bind-room --mentions-only`
// but outside company membership therefore received nothing: neither
// @mentions nor follow-ups in its own threads (citadel gate r3 MAJOR at
// main.go:4058, 2026-09-17). The gateway now runs the same lane itself,
// right after admission:
//
//   - once per message in effect: the per-(session, channel, ts) claim
//     inside deliverMentionOnly collapses every copy of an origin — the
//     first admission, Slack redeliveries (duplicate receipt) and the
//     app_mention twin all enter the lane, and only an UNCOMMITTED claim
//     (no delivery yet, or a failed one that released it) proceeds. A
//     failed injection is also retried in place on a bounded backoff
//     (companyMentionOnlyRetryBackoff), because unlike the legacy path
//     the company receipt is already acked and admitted, so Slack's
//     retry ladder is not a reliable second chance here (codex r5 P1).
//     The claim is in memory and lives 10 minutes; its durable half is
//     the receipt's MentionOnlyDelivered list, consulted on every entry,
//     so a Slack redelivery after a restart (or after the claim's TTL)
//     does not wake the session a second time (round 6). The list is
//     written after the injection: a crash between gc's accept and that
//     write can still deliver twice — at-least-once, never silently zero.
//     Injections still failed when a run ends (ladder exhausted, or cut
//     short by shutdown) are left on the receipt as MentionOnlyPending
//     and replayed after the next startup recovery (codex r6 P1);
//   - only for human-authored, admissible messages (bot posts are never
//     an ask — the session's own replies through /publish must not wake
//     it in its own thread);
//   - never for a mention-only binding whose session IS a company member
//     of the room: the gateway already delivers that message to it, and
//     a second copy would be the double delivery this lane exists to
//     avoid;
//   - with the same selection rules as the legacy lane
//     (selectMentionOnlyTargets): bot @mention, `@handle:` prefix naming
//     the binding's handle, or a reply in a thread the session posted in
//     (own-thread registry, Slack scan fallback for pre-registry threads).
//     "The bot" is the SWITCHBOARD's bot user: a company room's copies can
//     be delivered by persona apps too, whose envelope names the persona's
//     bot, so the mention is pinned on SLACK_APP_ID /
//     SLACK_SWITCHBOARD_BOT_USER_ID when configured (switchboardBot).
//
// Channels without mention-only bindings take exactly the path they took
// before this file existed. The lane runs asynchronously under
// deliverWG (tests wait on it) and mentionOnlyWG, which shutdown joins
// (main.go step 3b) once cfg.draining has stopped the retry ladder; it
// does not hold a dispatch slot (the company receipt's delivery holds
// one) — the injection set is bounded by the room's mention-only bindings.

// mentionOnlyForCompanyRoom decides whether the lane has anything to do
// for an admitted company-room message and, if so, runs it in the
// background. room is the directory entry the admission matched.
func (g *companyGateway) mentionOnlyForCompanyRoom(env slackEventEnvelope, ev slackMessageEvent, room *CompanyRoom) {
	if g == nil || room == nil || g.cfg.mentionOnly == nil {
		return
	}
	bindings := g.cfg.mentionOnly.ForChannel(ev.Channel)
	if len(bindings) == 0 {
		return
	}
	// A bot post is never an ask (mirrors processSlackEvent's peer-bot
	// branch): the session's own threaded replies land here as bot
	// posts, and delivering them back would wake it on itself.
	if ev.BotID != "" || ev.Subtype == "bot_message" || ev.User == "" {
		return
	}
	// Company members are the gateway's own audience; a mention-only
	// binding for one of them is redundant here, never a second copy.
	members := g.companyMemberSessions(room)
	outsiders := bindings[:0:0]
	for _, b := range bindings {
		if isCompanyMemberBinding(b, members) {
			log.Printf("company: mention-only binding session=%s chan=%s is a company member of room %s — gateway delivery covers it, lane skipped",
				b.SessionID, ev.Channel, room.Name)
			continue
		}
		outsiders = append(outsiders, b)
	}
	if len(outsiders) == 0 {
		return
	}
	g.deliverWG.Add(1)
	g.mentionOnlyWG.Add(1)
	go func() {
		defer g.deliverWG.Done()
		defer g.mentionOnlyWG.Done()
		g.deliverMentionOnlyForCompanyRoom(env, ev, outsiders)
	}()
}

// switchboardBot returns the bot user id an @mention must name to count
// as "the bot was mentioned", and whether this copy of the event was
// delivered by the switchboard app. With SLACK_APP_ID unset every copy is
// taken as the switchboard's (the documented manifest split: only the
// switchboard subscribes app_mention); with it set, a persona app's copy
// contributes neither its app_mention type nor its own bot user id.
func (g *companyGateway) switchboardBot(env slackEventEnvelope) (botUserID string, fromSwitchboard bool) {
	fromSwitchboard = g.cfg.slackAppID == "" || env.APIAppID == g.cfg.slackAppID
	botUserID = g.cfg.companySelfBotUserID
	if botUserID == "" && fromSwitchboard {
		botUserID = env.botUserID()
	}
	return botUserID, fromSwitchboard
}

// mentionOnlyAlreadyReached drops the targets the receipt durably records
// as delivered. No store, no receipt yet (the app_mention twin can outrun
// the message copy) or a read error all leave the in-memory claim as the
// only guard — exactly the pre-round-6 behavior.
func (g *companyGateway) mentionOnlyAlreadyReached(origin ReceiptOrigin, targets []mentionOnlyTarget) []mentionOnlyTarget {
	store := g.store()
	if store == nil {
		return targets
	}
	r, err := store.Get(origin)
	if err != nil || r == nil || len(r.MentionOnlyDelivered) == 0 {
		return targets
	}
	reached := make(map[string]bool, len(r.MentionOnlyDelivered))
	for _, sid := range r.MentionOnlyDelivered {
		reached[sid] = true
	}
	out := targets[:0:0]
	for _, t := range targets {
		if reached[t.binding.SessionID] {
			log.Printf("company: mention-only lane session=%s chan=%s ts=%s already delivered per the durable receipt — skipped",
				t.binding.SessionID, origin.ChannelID, origin.TS)
			continue
		}
		out = append(out, t)
	}
	return out
}

// recordMentionOnlyOutcome writes a lane run's state onto the receipt in
// one generation-checked commit (the company delivery commits to the same
// receipt) — called once before dispatch with every target as pending, and
// once after with the CONFIRMED results: reached sessions join MentionOnlyDelivered
// and leave MentionOnlyPending; still-failed targets join
// MentionOnlyPending unless another copy has reached them meanwhile. Only
// injections this run saw gc accept count as reached — the delivery log is
// provisional (written before the POST) and is never read as proof (codex
// r6 P2). Best-effort: a failed write leaves the in-memory claim as the
// only guard, as before round 6.
func (g *companyGateway) recordMentionOnlyOutcome(origin ReceiptOrigin, reached, failed []mentionOnlyTarget, waitForReceipt bool) {
	store := g.store()
	if store == nil || (len(reached) == 0 && len(failed) == 0) {
		return
	}
	var r *IngressReceipt
	// The app_mention twin can outrun the message copy that creates the
	// receipt; after a delivery, give admission a moment before giving up
	// (the pre-dispatch write never delays the injection for it).
	attempts := 1
	if waitForReceipt {
		attempts = 4
	}
	for attempt := 0; attempt < attempts; attempt++ {
		var err error
		if r, err = store.Get(origin); err != nil {
			log.Printf("company: mention-only lane chan=%s ts=%s receipt read failed (%v) — outcome not recorded", origin.ChannelID, origin.TS, err)
			return
		}
		if r != nil || attempt == attempts-1 {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if r == nil {
		log.Printf("company: mention-only lane chan=%s ts=%s has no receipt — outcome not recorded", origin.ChannelID, origin.TS)
		return
	}
	merge := func(cur *IngressReceipt) {
		done := make(map[string]bool, len(cur.MentionOnlyDelivered)+len(reached))
		for _, sid := range cur.MentionOnlyDelivered {
			done[sid] = true
		}
		for _, t := range reached {
			if !done[t.binding.SessionID] {
				done[t.binding.SessionID] = true
				cur.MentionOnlyDelivered = append(cur.MentionOnlyDelivered, t.binding.SessionID)
			}
		}
		sort.Strings(cur.MentionOnlyDelivered)
		pending := cur.MentionOnlyPending[:0:0]
		queued := make(map[string]bool)
		for _, pt := range cur.MentionOnlyPending {
			if !done[pt.SessionID] && !queued[pt.SessionID] {
				queued[pt.SessionID] = true
				pending = append(pending, pt)
			}
		}
		for _, t := range failed {
			if !done[t.binding.SessionID] && !queued[t.binding.SessionID] {
				queued[t.binding.SessionID] = true
				pending = append(pending, MentionOnlyPendingTarget{SessionID: t.binding.SessionID, Reason: t.reason})
			}
		}
		cur.MentionOnlyPending = pending
	}
	if err := g.commitReceipt(r, merge); err != nil {
		log.Printf("company: mention-only lane chan=%s ts=%s could not record its outcome on the receipt (%v) — the in-memory claim stays the only redelivery guard",
			origin.ChannelID, origin.TS, err)
	}
}

// confirmMentionOnlyAsync awaits the terminal result of a session.message
// request gc accepted asynchronously (HTTP 202), on the same durable event
// stream the company delivery uses. Only a delivered result counts; a
// failed or interrupted wait reads as a failed injection, so the claim is
// released and the target stays pending (retry ladder, then startup replay).
//
// An interrupted wait (stream error, disconnect, timeout) is NOT a failed
// request — the original may still deliver — so the same request is
// re-confirmed a bounded number of times before it is given up as failed
// and the ladder re-posts (which can then duplicate: at-least-once).
func (g *companyGateway) confirmMentionOnlyAsync(requestID, eventCursor string) (string, bool) {
	td := TargetDelivery{RequestID: requestID, EventCursor: eventCursor}
	res := g.awaitCompanyMessageResult(td)
	for attempt := 0; res.disposition == postRetryable && attempt < companyMentionOnlyConfirmRetries; attempt++ {
		if g.cfg.draining != nil && g.cfg.draining.Load() {
			break
		}
		log.Printf("company: mention-only async request %s: confirmation interrupted (%s) — re-confirming, not re-posting (attempt %d/%d)",
			requestID, res.detail, attempt+1, companyMentionOnlyConfirmRetries)
		res = g.awaitCompanyMessageResult(td)
	}
	return res.detail, res.disposition == postDelivered
}

// companyMentionOnlyConfirmRetries bounds the re-confirmations of one
// accepted request after an interrupted event-stream wait.
const companyMentionOnlyConfirmRetries = 3

// companyMentionOnlyReplayMaxAge bounds the startup replay: an injection
// still pending on a receipt older than this is left alone (and ages out
// with the receipt) rather than waking a session about a stale message.
const companyMentionOnlyReplayMaxAge = 24 * time.Hour

// replayMentionOnlyPending re-runs the lane for every receipt that still
// lists pending mention-only injections (codex r6 P1): the event was acked
// at admission, Slack owes no redelivery, and the company sweep drives only
// the company delivery. Called once after startup recovery opens the
// barrier. A pending session whose binding has since been removed — or
// that became a company member of the room — is dropped from the list.
func (g *companyGateway) replayMentionOnlyPending() {
	if g == nil || g.cfg.mentionOnly == nil {
		return
	}
	store := g.store()
	if store == nil {
		return
	}
	receipts, err := store.List()
	if err != nil {
		log.Printf("company: mention-only replay scan failed: %v", err)
		return
	}
	for _, r := range receipts {
		if len(r.MentionOnlyPending) == 0 {
			continue
		}
		if g.now().Sub(r.ReceivedAt) > companyMentionOnlyReplayMaxAge {
			continue
		}
		var ev slackMessageEvent
		if err := json.Unmarshal(store.receiptBody(r), &ev); err != nil || ev.Channel == "" || ev.TS == "" {
			log.Printf("company: mention-only replay %s: event body unavailable — %d pending injection(s) left", r.ID, len(r.MentionOnlyPending))
			continue
		}
		var members map[string]bool
		if room, ok := g.dirStore.Snapshot().RoomByChannel(r.Origin.TeamID, ev.Channel); ok {
			members = g.companyMemberSessions(room)
		}
		bindings := g.cfg.mentionOnly.ForChannel(ev.Channel)
		var targets []mentionOnlyTarget
		for _, pt := range r.MentionOnlyPending {
			for _, b := range bindings {
				if b.matchesSession(pt.SessionID) && !isCompanyMemberBinding(b, members) {
					targets = append(targets, mentionOnlyTarget{binding: b, reason: pt.Reason})
					break
				}
			}
		}
		if dropped := len(r.MentionOnlyPending) - len(targets); dropped > 0 {
			keep := make(map[string]bool, len(targets))
			for _, t := range targets {
				keep[t.binding.SessionID] = true
			}
			if err := g.commitReceipt(r, func(cur *IngressReceipt) {
				kept := cur.MentionOnlyPending[:0:0]
				for _, pt := range cur.MentionOnlyPending {
					if keep[pt.SessionID] {
						kept = append(kept, pt)
					}
				}
				cur.MentionOnlyPending = kept
			}); err != nil {
				log.Printf("company: mention-only replay %s: could not drop %d unbound pending injection(s): %v", r.ID, dropped, err)
			}
		}
		if len(targets) == 0 {
			continue
		}
		log.Printf("company: mention-only replay chan=%s ts=%s — %d injection(s) left pending by the previous run", ev.Channel, ev.TS, len(targets))
		origin, ev, targets := r.Origin, ev, targets
		g.deliverWG.Add(1)
		g.mentionOnlyWG.Add(1)
		go func() {
			defer g.deliverWG.Done()
			defer g.mentionOnlyWG.Done()
			explicit, _ := parseHandlePrefix(ev.Text, g.cfg.handlePrefix)
			g.runMentionOnlyTargets(origin, ev, explicit, targets)
		}()
	}
}

// sleepUnlessDraining waits d in short slices and gives up (false) as soon
// as shutdown begins, so the retry ladder never holds shutdown for a full
// backoff step.
func sleepUnlessDraining(cfg config, d time.Duration) bool {
	const slice = 200 * time.Millisecond
	for d > 0 {
		if cfg.draining != nil && cfg.draining.Load() {
			return false
		}
		step := d
		if step > slice {
			step = slice
		}
		time.Sleep(step)
		d -= step
	}
	return cfg.draining == nil || !cfg.draining.Load()
}

// companyMentionOnlyRetryBackoff paces the in-place retries of a failed
// company-room mention-only injection (codex r5 P1). Package-level so
// tests can shorten it.
var companyMentionOnlyRetryBackoff = []time.Duration{2 * time.Second, 10 * time.Second, 30 * time.Second}

// companyMemberSessions returns every LOCAL-city session the bindings
// snapshot resolves for the room's declared members. A binding that
// targets another city names a session in that city's namespace; a local
// mention-only binding with the same name is a different session, and
// the gateway delivers only to the remote one — so remote bindings never
// enter the exclusion set (codex r5 P2). Overlay (roster-drift) agents
// resolve through dm_bindings only at delivery time and are not part of
// this set; a mention-only binding for one of those would be delivered
// twice — a known, logged edge (both copies carry the ts).
func (g *companyGateway) companyMemberSessions(room *CompanyRoom) map[string]bool {
	out := make(map[string]bool)
	if g == nil || room == nil || g.bindStore == nil {
		return out
	}
	bindings := g.bindStore.Snapshot()
	for _, agent := range room.Members {
		bd, ok := bindings.BindingFor(room.Name, agent)
		if !ok || bd == nil || bd.Session == "" {
			continue
		}
		if bd.City != "" && bd.City != g.cfg.cityName {
			continue
		}
		out[bd.Session] = true
	}
	return out
}

func isCompanyMemberBinding(b mentionOnlyBinding, members map[string]bool) bool {
	for session := range members {
		if b.matchesSession(session) {
			return true
		}
	}
	return false
}

// deliverMentionOnlyForCompanyRoom is the lane body: selection, then the
// shared injection path (deliverMentionOnly). Runs in its own goroutine.
func (g *companyGateway) deliverMentionOnlyForCompanyRoom(env slackEventEnvelope, ev slackMessageEvent, bindings []mentionOnlyBinding) {
	cfg := g.cfg
	isThreadReply := ev.ThreadTS != "" && ev.ThreadTS != ev.TS
	// The `@handle:` prefix is the only address form resolved here: User
	// Group mentions and thread-sticky handles need the alias registries
	// the legacy dispatcher owns, and a company room's mention-only
	// audience is reached by bot @mention or handle prefix in practice.
	target, _ := parseHandlePrefix(ev.Text, cfg.handlePrefix)
	botUID, fromSwitchboard := g.switchboardBot(env)
	in := mentionOnlyInput{
		bindings:      bindings,
		botMentioned:  (ev.Type == "app_mention" && fromSwitchboard) || slackTextMentionsUser(ev.Text, botUID),
		target:        target,
		isThreadReply: isThreadReply,
	}
	if isThreadReply {
		in.threadPosters, in.threadKnown = cfg.ownThreads.posters(ev.Channel, ev.ThreadTS)
		if !in.threadKnown {
			// Pre-registry thread: scan it once for the adapter's own bot
			// user, exactly like the legacy lane's fallback.
			if botUID != "" && cfg.slackBotToken != "" {
				fetchCtx, cancel := context.WithTimeout(context.Background(), threadContextFetchTimeout)
				replies, err := fetchThreadReplies(fetchCtx, cfg.slackBotToken, ev.Channel, ev.ThreadTS, ownThreadScanLimit)
				cancel()
				if err != nil {
					log.Printf("company: mention-only own-thread scan fetch failed chan=%s thread=%s: %v", ev.Channel, ev.ThreadTS, err)
				} else {
					in.botPostedInThread = threadHasOwnBotPost(replies, botUID, ev.TS)
				}
			}
		}
	}
	origin := ReceiptOrigin{TeamID: env.TeamID, ChannelID: ev.Channel, TS: ev.TS}
	g.runMentionOnlyTargets(origin, ev, target, selectMentionOnlyTargets(in))
}

// runMentionOnlyTargets injects the selected targets (minus the ones the
// receipt durably records as reached), retries failures in place, and
// records the confirmed outcome on the receipt. Shared by the live lane and
// the startup replay of pending injections.
func (g *companyGateway) runMentionOnlyTargets(origin ReceiptOrigin, ev slackMessageEvent, explicitTarget string, targets []mentionOnlyTarget) {
	cfg := g.cfg
	targets = g.mentionOnlyAlreadyReached(origin, targets)
	if len(targets) == 0 {
		return
	}
	text := rewriteSlackUserMentions(cfg, ev.Text)
	// Files are not downloaded on this path (the company hydrator owns
	// file rendering for members); the reminder still names every file.
	if filesBlock := formatInboundFilesBlock(ev.Files, nil); filesBlock != "" {
		if strings.TrimSpace(text) == "" {
			text = filesBlock
		} else {
			text += "\n\n" + filesBlock
		}
	}
	inbound := externalInboundMessage{
		ProviderMessageID: ev.TS,
		Conversation: conversationRef{
			ScopeID:        cfg.cityName,
			Provider:       cfg.provider,
			AccountID:      cfg.accountID,
			ConversationID: ev.Channel,
			Kind:           "room",
		},
		Actor: externalActor{
			ID:          ev.User,
			DisplayName: resolveUserDisplayName(cfg, ev.User),
			IsBot:       false,
		},
		Text:             text,
		ExplicitTarget:   explicitTarget,
		ReplyToMessageID: ev.ThreadTS,
		DedupKey:         "slack-" + ev.TS,
		ReceivedAt:       time.Now().UTC(),
	}
	// Every selected target is written to the receipt as PENDING before the
	// first POST (codex r6 re-gate P1): a process that exits mid-run — a
	// slow gc can outlast shutdown's bounded join — leaves the work where
	// the startup replay finds it. The outcome write below clears what was
	// reached. Costs a possible second delivery when the exit falls between
	// gc's accept and that write: at-least-once, never silently zero.
	g.recordMentionOnlyOutcome(origin, nil, targets, false)
	// settled = skipped on a committed claim: the app_mention twin (or a
	// redelivery) delivered it. It is recorded as reached here too, because
	// a twin that outran the message copy had no receipt to write to.
	reached, failed, settled := deliverMentionOnlyOutcomes(cfg, targets, inbound, g.confirmMentionOnlyAsync)
	reached = append(reached, settled...)
	// A failed injection released its claim; re-post the failed ones in
	// place on a bounded backoff. The receipt is already admitted and
	// acked, so nothing upstream retries for us; a twin or duplicate
	// delivery arriving meanwhile takes the released claim itself, this
	// loop then skips on its committed claim and that copy records it.
	for attempt, wait := range companyMentionOnlyRetryBackoff {
		if len(failed) == 0 {
			break
		}
		if !sleepUnlessDraining(cfg, wait) {
			log.Printf("company: mention-only lane chan=%s ts=%s %d injection(s) still failed at shutdown — left pending on the receipt for the startup replay", ev.Channel, ev.TS, len(failed))
			break
		}
		log.Printf("company: mention-only lane chan=%s ts=%s retrying %d failed injection(s) (attempt %d/%d)",
			ev.Channel, ev.TS, len(failed), attempt+1, len(companyMentionOnlyRetryBackoff))
		var d, st []mentionOnlyTarget
		d, failed, st = deliverMentionOnlyOutcomes(cfg, failed, inbound, g.confirmMentionOnlyAsync)
		reached = append(append(reached, d...), st...)
	}
	g.recordMentionOnlyOutcome(origin, reached, failed, true)
	if len(failed) > 0 {
		log.Printf("company: mention-only lane chan=%s ts=%s UNDELIVERED %d injection(s) — message marked ⚠️, left pending on the receipt; the app_mention twin, a Slack redelivery or the next startup replay can still deliver them",
			ev.Channel, ev.TS, len(failed))
	}
	log.Printf("company: mention-only lane chan=%s ts=%s thread=%s delivered=%d failed=%d (company room; members delivered by the gateway)",
		ev.Channel, ev.TS, ev.ThreadTS, len(reached), len(failed))
}

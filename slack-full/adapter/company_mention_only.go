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
//     and replayed after the next startup recovery (codex r6 P1). The
//     lane's intent is durable even earlier: the admission write that
//     creates the receipt — before the Slack ack — carries
//     MentionOnlyLane whenever the room has outsider bindings, and a
//     process that exits before the lane has recorded its selection has
//     the whole lane re-run at the next startup (citadel gate r7);
//   - KNOWN LIMIT — gc's four-minute cap on session.message: gc concludes
//     an injection at the session's next idle boundary and, when the
//     session stays busy past four minutes, emits request.failed with
//     code "timeout" (gascity huma_handlers_sessions_command.go /
//     client.go). The adapter reads that event as definitive (a failed
//     injection), so a session busy longer than that looks undelivered:
//     the ladder re-POSTs up to three more times over ~17 minutes and the
//     message is marked ⚠️, while gc may still hold — and later process —
//     one or more of the cancelled copies (duplicates), or may have
//     dropped the line. Whether it queues or abandons them is not
//     verified; the owner runs one busy-session test before a city whose
//     sessions run long turns adopts this pack. The closing shape is to
//     treat gc's timeout code as PENDING (re-confirm the same request
//     rather than re-post it), or a gc-side terminal event that tells
//     queued from failed — partly gc's home, so not changed here (citadel
//     read r3 MINOR 1);
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
//     MULTI-APP LAYOUT (SLACK_APP_ID set, persona apps subscribed to the
//     room's message events): set SLACK_SWITCHBOARD_BOT_USER_ID. Without it
//     a persona copy cannot recognize a switchboard @mention; the layout
//     stays safe — such a copy never settles the admission intent, the
//     switchboard's copy takes the intent over before its ack
//     (adoptMentionOnlyIntent) and only a run that can recognize the
//     mention, live or replayed, settles it (citadel gate r8) — at a cost:
//     in a room the switchboard app delivers no copy for, every human
//     message keeps its intent and is re-selected at each startup until the
//     replay's 24-hour bound.
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
	outsiders := g.mentionOnlyOutsiders(ev, room, true)
	if len(outsiders) == 0 {
		return
	}
	g.deliverWG.Add(1)
	g.mentionOnlyWG.Add(1)
	go func() {
		defer g.deliverWG.Done()
		defer g.mentionOnlyWG.Done()
		if g.mentionOnlyLaneStart != nil && !g.mentionOnlyLaneStart() {
			return
		}
		g.deliverMentionOnlyForCompanyRoom(env, ev, outsiders)
	}()
}

// mentionOnlyOutsiders returns the room's mention-only bindings the lane
// serves for ev: none for a bot post, and never a binding whose session is
// a company member of the room. Also asked at admission (verbose false),
// to decide whether the receipt must carry the lane's replay intent.
func (g *companyGateway) mentionOnlyOutsiders(ev slackMessageEvent, room *CompanyRoom, verbose bool) []mentionOnlyBinding {
	if g == nil || room == nil || g.cfg.mentionOnly == nil {
		return nil
	}
	bindings := g.cfg.mentionOnly.ForChannel(ev.Channel)
	if len(bindings) == 0 {
		return nil
	}
	// A bot post is never an ask (mirrors processSlackEvent's peer-bot
	// branch): the session's own threaded replies land here as bot
	// posts, and delivering them back would wake it on itself.
	if ev.BotID != "" || ev.Subtype == "bot_message" || ev.User == "" {
		return nil
	}
	// Company members are the gateway's own audience; a mention-only
	// binding for one of them is redundant here, never a second copy.
	members := g.companyMemberSessions(room)
	outsiders := bindings[:0:0]
	for _, b := range bindings {
		if isCompanyMemberBinding(b, members) {
			if verbose {
				log.Printf("company: mention-only binding session=%s chan=%s is a company member of room %s — gateway delivery covers it, lane skipped",
					b.SessionID, ev.Channel, room.Name)
			}
			continue
		}
		outsiders = append(outsiders, b)
	}
	return outsiders
}

// mentionOnlyAdmissionIntent is the replay intent the admission write puts
// on a new receipt (nil when the lane has nothing to do for ev): the lane
// itself runs only after the Slack ack, so this is what a process that
// exits in between leaves for the startup replay (citadel gate r7).
func (g *companyGateway) mentionOnlyAdmissionIntent(env slackEventEnvelope, ev slackMessageEvent, room *CompanyRoom) *MentionOnlyLaneIntent {
	if len(g.mentionOnlyOutsiders(ev, room, false)) == 0 {
		return nil
	}
	return &MentionOnlyLaneIntent{AppID: env.APIAppID, BotUserID: env.botUserID()}
}

// adoptMentionOnlyIntent runs on the duplicate branch, BEFORE the ack: when
// this copy is the switchboard app's and the unsettled intent on the receipt
// carries another app's identity (a persona copy was admitted first), the
// intent takes the switchboard's — so a process that exits after this ack has
// the startup replay run AS the switchboard copy, the only one that
// recognizes a switchboard @mention while SLACK_SWITCHBOARD_BOT_USER_ID is
// unset (citadel read r4 MINOR 1). An error means the caller answers 503.
func (g *companyGateway) adoptMentionOnlyIntent(env slackEventEnvelope, existing *IngressReceipt) error {
	if g.cfg.slackAppID == "" || env.APIAppID != g.cfg.slackAppID ||
		existing == nil || existing.MentionOnlyLane == nil || existing.MentionOnlyLane.AppID == env.APIAppID {
		return nil
	}
	return g.commitReceipt(existing, func(cur *IngressReceipt) {
		// Settled meanwhile by a run that could recognize the mention: stays so.
		if cur.MentionOnlyLane != nil {
			cur.MentionOnlyLane = &MentionOnlyLaneIntent{AppID: env.APIAppID, BotUserID: env.botUserID()}
		}
	})
}

// settleMentionOnlyIntent clears the admission intent of a lane run that
// has nothing to record (nobody selected, or everyone already reached). A
// run with targets clears it in the commit that writes them as pending
// (recordMentionOnlyOutcome). No receipt (the app_mention twin outran the
// message copy) or no intent: nothing to write.
func (g *companyGateway) settleMentionOnlyIntent(origin ReceiptOrigin) {
	store := g.store()
	if store == nil {
		return
	}
	r, err := store.Get(origin)
	if err != nil || r == nil || r.MentionOnlyLane == nil {
		return
	}
	if err := g.commitReceipt(r, func(cur *IngressReceipt) { cur.MentionOnlyLane = nil }); err != nil {
		log.Printf("company: mention-only lane chan=%s ts=%s could not settle its admission intent (%v) — the next startup re-runs the selection",
			origin.ChannelID, origin.TS, err)
	}
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
	// Matched across the binding's identifiers: a binding re-made under
	// another identifier (name -> id) must still find the marker written
	// under the old one (codex r7 P2).
	reached := func(b mentionOnlyBinding) bool {
		for _, sid := range r.MentionOnlyDelivered {
			if b.matchesSession(sid) {
				return true
			}
		}
		return false
	}
	out := targets[:0:0]
	for _, t := range targets {
		if reached(t.binding) {
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
func (g *companyGateway) recordMentionOnlyOutcome(origin ReceiptOrigin, reached, failed []mentionOnlyTarget, waitForReceipt, keepIntent bool) {
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
		// The selection is on the receipt from here on: the admission
		// intent has done its job — unless the selection is unfinished,
		// when the next startup must run it again (keepIntent).
		if !keepIntent {
			cur.MentionOnlyLane = nil
		}
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
		// A pending entry is matched across the binding's identifiers, like
		// the replay that resolves it: written under the session's name, it
		// must still leave when the binding re-made under the gc id is the
		// one reached (citadel gate r7 Minor).
		reachedAs := func(sid string) bool {
			if done[sid] {
				return true
			}
			for _, t := range reached {
				if t.binding.matchesSession(sid) {
					return true
				}
			}
			return false
		}
		pending := cur.MentionOnlyPending[:0:0]
		queued := make(map[string]bool)
		for _, pt := range cur.MentionOnlyPending {
			if !reachedAs(pt.SessionID) && !queued[pt.SessionID] {
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
		if len(r.MentionOnlyPending) == 0 && r.MentionOnlyLane == nil {
			continue
		}
		if g.now().Sub(r.ReceivedAt) > companyMentionOnlyReplayMaxAge {
			continue
		}
		var ev slackMessageEvent
		if err := json.Unmarshal(store.receiptBody(r), &ev); err != nil || ev.Channel == "" || ev.TS == "" {
			log.Printf("company: mention-only replay %s: event body unavailable — %d pending injection(s) left (unsettled admission intent: %t)",
				r.ID, len(r.MentionOnlyPending), r.MentionOnlyLane != nil)
			continue
		}
		if r.MentionOnlyLane != nil {
			// The previous run exited between the ack and the lane's first
			// write (citadel gate r7), or left its selection unfinished (failed
			// own-thread scan, a persona copy that cannot see a switchboard
			// mention): the WHOLE lane runs again — selection included — on an
			// envelope rebuilt from the receipt. A finished selection's pending
			// write clears the intent; an unfinished one keeps it BESIDE its
			// pending entries, and this re-run selects those bindings again
			// (sessions already reached are skipped; an entry it does not
			// select again stays pending for the startup after).
			g.replayMentionOnlyLane(r, ev)
			continue
		}
		var members map[string]bool
		if room, ok := g.dirStore.Snapshot().RoomByChannel(r.Origin.TeamID, ev.Channel); ok {
			members = g.companyMemberSessions(room)
		}
		bindings := g.cfg.mentionOnly.ForChannel(ev.Channel)
		var targets []mentionOnlyTarget
		// keep holds the pending entries' OWN identifiers: an entry written
		// under a session's old name and matched by its rebound binding must
		// survive the prune below, or a restart before the replay's pending
		// write loses it (codex r8 P2).
		keep := make(map[string]bool, len(r.MentionOnlyPending))
		for _, pt := range r.MentionOnlyPending {
			for _, b := range bindings {
				if b.matchesSession(pt.SessionID) && !isCompanyMemberBinding(b, members) {
					targets = append(targets, mentionOnlyTarget{binding: b, reason: pt.Reason})
					keep[pt.SessionID] = true
					break
				}
			}
		}
		if dropped := len(r.MentionOnlyPending) - len(targets); dropped > 0 {
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
			explicit, _ := resolveAddressTarget(g.cfg, g.cfg.handleAliases, g.cfg.subteamAliases, ev.Text)
			g.runMentionOnlyTargets(origin, ev, explicit, targets, false)
		}()
	}
}

// replayMentionOnlyLane re-enters the live lane for a receipt whose
// admission intent was never settled. The envelope carries what selection
// reads: the team, the delivering app, and that app's bot user.
func (g *companyGateway) replayMentionOnlyLane(r *IngressReceipt, ev slackMessageEvent) {
	room, ok := g.dirStore.Snapshot().RoomByChannel(r.Origin.TeamID, ev.Channel)
	if !ok || len(g.mentionOnlyOutsiders(ev, room, false)) == 0 {
		// No longer a company room, or no outsider binding left to serve.
		g.settleMentionOnlyIntent(r.Origin)
		return
	}
	env := slackEventEnvelope{Type: "event_callback", TeamID: r.Origin.TeamID, APIAppID: r.APIAppID, EventID: r.EventID}
	if r.MentionOnlyLane.AppID != "" {
		env.APIAppID = r.MentionOnlyLane.AppID
	}
	if r.MentionOnlyLane.BotUserID != "" {
		env.Authorizations = []slackEventAuthorization{{UserID: r.MentionOnlyLane.BotUserID, IsBot: true}}
	}
	log.Printf("company: mention-only replay chan=%s ts=%s — the previous run exited before the lane recorded its selection; running it again", ev.Channel, ev.TS)
	g.mentionOnlyForCompanyRoom(env, ev, room)
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
// mention-only injection (codex r5 P1) — in both lanes since round 7: the
// legacy lane's event is acked to Slack before it runs too, so nothing
// upstream retries for it either (retryMentionOnlyFailed). Package-level
// so tests can shorten it.
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
	// The legacy dispatcher's address resolution (`@handle:` prefix, labeled
	// and mapped User Group mentions — resolveAddressTarget), so a session
	// is addressed the same way in either kind of room. Thread-sticky
	// handles stay legacy-only: they are written by the alias dispatcher,
	// which never runs for a company room. No alias leg here either, so a
	// binding matched through the alias registry is always this lane's to
	// deliver (aliasLegDelivers stays false).
	target, _ := resolveAddressTarget(cfg, cfg.handleAliases, cfg.subteamAliases, ev.Text)
	botUID, fromSwitchboard := g.switchboardBot(env)
	// A persona app's copy with no switchboard bot user configured cannot
	// recognize a switchboard @mention (nor scan for its posts): what it
	// selects is delivered, but its selection is never complete, so it must
	// not settle the admission intent it shares with the switchboard's copy
	// (citadel gate r8 Major).
	blindCopy := !fromSwitchboard && botUID == ""
	in := mentionOnlyInput{
		bindings:      bindings,
		botMentioned:  (ev.Type == "app_mention" && fromSwitchboard) || slackTextMentionsUser(ev.Text, botUID),
		target:        target,
		isThreadReply: isThreadReply,
	}
	if target != "" && cfg.handleAliases != nil {
		in.aliasedSessionID, _ = cfg.handleAliases.Get(target)
	}
	scanFailed := false
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
					scanFailed = true
				} else {
					in.botPostedInThread = threadHasOwnBotPost(replies, botUID, ev.TS)
				}
			}
		}
	}
	origin := ReceiptOrigin{TeamID: env.TeamID, ChannelID: ev.Channel, TS: ev.TS}
	targets := selectMentionOnlyTargets(in)
	if scanFailed {
		// A failed scan is not evidence that the bot never posted: the
		// selection is unfinished, so the admission intent stays on the
		// receipt — whatever WAS selected (a handle, a bot mention) is
		// delivered now — and the next startup runs the lane, scan
		// included, again instead of settling a follow-up nobody else will
		// redeliver (codex r8 P2). Sessions reached now are skipped then.
		log.Printf("company: mention-only lane chan=%s ts=%s thread=%s selection unfinished (own-thread scan failed) — admission intent left for the startup replay",
			ev.Channel, ev.TS, ev.ThreadTS)
		if len(targets) == 0 {
			return
		}
	}
	if blindCopy && len(targets) == 0 {
		return
	}
	g.runMentionOnlyTargets(origin, ev, target, targets, scanFailed || blindCopy)
}

// runMentionOnlyTargets injects the selected targets (minus the ones the
// receipt durably records as reached), retries failures in place, and
// records the confirmed outcome on the receipt. Shared by the live lane and
// the startup replay of pending injections.
//
// selectionUnfinished says the caller could not finish selecting (own-thread
// scan failed): the outcome writes then leave the admission intent in place.
func (g *companyGateway) runMentionOnlyTargets(origin ReceiptOrigin, ev slackMessageEvent, explicitTarget string, targets []mentionOnlyTarget, selectionUnfinished bool) {
	cfg := g.cfg
	targets = g.mentionOnlyAlreadyReached(origin, targets)
	if len(targets) == 0 {
		if !selectionUnfinished {
			g.settleMentionOnlyIntent(origin)
		}
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
	g.recordMentionOnlyOutcome(origin, nil, targets, false, selectionUnfinished)
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
	if len(failed) > 0 {
		var d []mentionOnlyTarget
		d, failed = retryMentionOnlyFailed(cfg, "company: mention-only lane", failed, inbound, g.confirmMentionOnlyAsync)
		reached = append(reached, d...)
	}
	g.recordMentionOnlyOutcome(origin, reached, failed, true, selectionUnfinished)
	if len(failed) > 0 {
		log.Printf("company: mention-only lane chan=%s ts=%s UNDELIVERED %d injection(s) — message marked ⚠️, left pending on the receipt; the app_mention twin, a Slack redelivery or the next startup replay can still deliver them",
			ev.Channel, ev.TS, len(failed))
	}
	log.Printf("company: mention-only lane chan=%s ts=%s thread=%s delivered=%d failed=%d (company room; members delivered by the gateway)",
		ev.Channel, ev.TS, ev.ThreadTS, len(reached), len(failed))
}

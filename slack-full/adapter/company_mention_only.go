package main

import (
	"context"
	"log"
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
//     retry ladder is not a reliable second chance here (codex r5 P1);
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
//
// Channels without mention-only bindings take exactly the path they took
// before this file existed. The lane runs asynchronously under
// deliverWG so shutdown joins it; it does not hold a dispatch slot (the
// company receipt's delivery holds one) — the injection set is bounded by
// the room's mention-only bindings.

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
	go func() {
		defer g.deliverWG.Done()
		g.deliverMentionOnlyForCompanyRoom(env, ev, outsiders)
	}()
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
	in := mentionOnlyInput{
		bindings:      bindings,
		botMentioned:  ev.Type == "app_mention" || slackTextMentionsUser(ev.Text, env.botUserID()),
		target:        target,
		isThreadReply: isThreadReply,
	}
	if isThreadReply {
		in.threadPosters, in.threadKnown = cfg.ownThreads.posters(ev.Channel, ev.ThreadTS)
		if !in.threadKnown {
			// Pre-registry thread: scan it once for the adapter's own bot
			// user, exactly like the legacy lane's fallback.
			if botUID := env.botUserID(); botUID != "" && cfg.slackBotToken != "" {
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
	targets := selectMentionOnlyTargets(in)
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
		ExplicitTarget:   target,
		ReplyToMessageID: ev.ThreadTS,
		DedupKey:         "slack-" + ev.TS,
		ReceivedAt:       time.Now().UTC(),
	}
	delivered, failed := deliverMentionOnly(cfg, targets, inbound)
	// A failed injection released its claim; re-run the lane in place on
	// a bounded backoff. Committed (delivered) targets skip on their
	// claim, so only the failed ones are re-posted. The receipt is
	// already admitted and acked, so nothing upstream retries for us;
	// a twin or duplicate delivery arriving meanwhile takes the released
	// claim itself and this loop finds it committed.
	for attempt, wait := range companyMentionOnlyRetryBackoff {
		if failed == 0 {
			break
		}
		if cfg.draining != nil && cfg.draining.Load() {
			log.Printf("company: mention-only lane chan=%s ts=%s %d injection(s) still failed at shutdown — not retried", ev.Channel, ev.TS, failed)
			break
		}
		time.Sleep(wait)
		log.Printf("company: mention-only lane chan=%s ts=%s retrying %d failed injection(s) (attempt %d/%d)",
			ev.Channel, ev.TS, failed, attempt+1, len(companyMentionOnlyRetryBackoff))
		var d int
		d, failed = deliverMentionOnly(cfg, targets, inbound)
		delivered += d
	}
	if failed > 0 {
		log.Printf("company: mention-only lane chan=%s ts=%s UNDELIVERED %d injection(s) after %d retries — message marked ⚠️; a Slack redelivery or the app_mention twin can still retake the released claim",
			ev.Channel, ev.TS, failed, len(companyMentionOnlyRetryBackoff))
	}
	log.Printf("company: mention-only lane chan=%s ts=%s thread=%s delivered=%d failed=%d (company room; members delivered by the gateway)",
		ev.Channel, ev.TS, ev.ThreadTS, delivered, failed)
}

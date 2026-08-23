package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// reaction_events.go — human-reaction visibility (gp-by3). Humans use
// emoji reactions as real conversational signals (ack, approval,
// emphasis) — Afik 👍-ing a mayor's question is often the ONLY answer
// that question gets — but the adapter historically had no
// reaction_added handling at all, so the bound session never saw them.
// The switchboard app now subscribes to reaction_added/reaction_removed
// (manifest change; the LIVE apps additionally need the subscription
// added in their Slack dashboard config) and forwards each surviving
// event to the conversation-bound session as a lightweight tagged
// notification: reactor, emoji, target message ts, and a best-effort
// text snippet of the target.
//
// Noise + loop rules (the fleet's own reaction traffic must not echo):
//  1. The adapter's OWN reactions never forward — the busy-affordance
//     hourglass (busy_reaction.go) fires reaction_added/removed for
//     every targeted inbound, and forwarding those back would wake the
//     session with its own machinery. Reactor == our bot user drops.
//  2. Any reaction using the configured busy emoji (cfg.busyReaction)
//     drops regardless of reactor: every fleet adapter in a shared
//     channel runs the same busy convention, so the emoji itself marks
//     mechanical traffic. (A human's rare genuine hourglass is the
//     accepted cost; a peer city configured with a DIFFERENT busy emoji
//     is only caught by rule 3.)
//  3. A reactor matching a peer-bot allowlist entry's bot_user_id
//     (peer_bots.json, gp-kop) drops — fleet peers ack mechanically.
//     Entries carrying only bot_id/app_id cannot be matched here
//     (reaction events carry user ids only); give fleet entries a
//     bot_user_id to filter their reactions.
//  4. No auto-response semantics: the notification is tagged in both
//     the Actor (IsBot=true, "reaction: " display prefix — the intake
//     helpers exclude it from reply-current/react/upload anchoring)
//     and the text ("no reply expected").
//
// Delivery NEVER wakes a session solo (gp-9e7 item 1 — without this,
// the reaction feature is a token regression): every surviving
// reaction, rooms and DMs alike, buffers in the coalescer's no-wake
// side lane and piggybacks on the channel's next real delivery — a
// coalesce window armed by messages, the reaction drain behind an
// urgent or DM inbound's committed delivery, or shutdown's drain.
// Exception: a reaction ON one of the adapter's own RECENT outbound
// messages (item_user == our bot, target ts within founderAckRecency —
// the founder acking a just-made post) may ride a coalesce window that
// is ALREADY armed; it still never arms one itself. Admission is final handling and the
// dedup claim commits either way, so a Slack redelivery cannot
// double-deliver; a failed batch flush restores the entry to the
// side-buffer for the next piggyback. Only a nil coalescer (bare test
// configs) falls back to an immediate forward, where failures are
// logged and dropped (reaction context is best-effort).
//
// Company rooms are out of scope v1: their message flow is gateway-
// owned, but reaction events fall through to this legacy path, so a
// company room's reactions forward only if the conversation also has a
// legacy binding. Per-agent DM reactions never arrive at all — the
// switchboard is not a member of those conversations.

// slackReactionEvent is the inner event payload of a
// reaction_added/reaction_removed event_callback.
type slackReactionEvent struct {
	Type     string `json:"type"`
	User     string `json:"user"`
	Reaction string `json:"reaction"`
	ItemUser string `json:"item_user,omitempty"`
	Item     struct {
		Type    string `json:"type"`
		Channel string `json:"channel,omitempty"`
		TS      string `json:"ts,omitempty"`
	} `json:"item"`
	EventTS string `json:"event_ts"`
}

// reactionActorPrefix tags the Actor.DisplayName of every forwarded
// reaction notification. slack_intake_common.py keys its event-scan
// exclusion on this prefix (same contract as peerActorPrefix) — keep
// the two in sync.
const reactionActorPrefix = "reaction: "

// maxReactionSnippetChars bounds the quoted target-message snippet.
// The snippet is orientation, not payload — a session that needs the
// full text can read the channel (`gc slack read`).
const maxReactionSnippetChars = 140

// reactionTargetFetchTimeout bounds the best-effort conversations.replies
// lookup for the target message's text; on expiry the notification
// simply carries no snippet.
const reactionTargetFetchTimeout = 4 * time.Second

// reactionTargetFetchLimit is the conversations.replies page for the
// target lookup. The target is normally the first message returned
// (thread parents and plain messages anchor the page; a mid-thread
// reply ts anchors at itself on current Slack behavior) — a small
// margin covers surface variance without paying for a real page.
const reactionTargetFetchLimit = 3

// founderAckRecency bounds how old the adapter's own reacted-to message
// may be for the reaction to count as a founder ack — the design's
// "reaction on our own RECENT message" (gp-9e7 item 1; enforcement
// added in the fix round, 1c). The ride-armed-window privilege exists
// for a human acking a post the adapter JUST made; a reaction on an
// arbitrarily old bot post is ordinary reaction traffic and must take
// the plain no-wake side-buffer. Slack message ts values carry their
// own epoch seconds, so the bound needs no outbound-history tracking.
const founderAckRecency = 15 * time.Minute

// slackTSMaxFutureSkew is the only future-ness slackTSWithin tolerates:
// small producer/consumer clock skew is real, but a ts further in the
// future than this is malformed or forged input, not a recent message
// — fail closed (gp-bhq E).
const slackTSMaxFutureSkew = time.Minute

// slackTSWithin reports whether ts — a Slack "seconds.sequence"
// message id — is at most maxAge old at now. Fail closed to the plain
// no-wake buffer, never to the founder-ack exception (gp-bhq E): the
// ts must be well-formed end to end — a "." separator with a numeric,
// non-empty fraction and positive seconds — and at most
// slackTSMaxFutureSkew in the future; anything else is false.
func slackTSWithin(ts string, maxAge time.Duration, now time.Time) bool {
	head, frac, ok := strings.Cut(ts, ".")
	if !ok || head == "" || frac == "" {
		return false
	}
	sec, err := strconv.ParseInt(head, 10, 64)
	if err != nil || sec <= 0 {
		return false
	}
	if _, err := strconv.ParseUint(frac, 10, 64); err != nil {
		return false
	}
	// Slack ts is decimal epoch time — the fraction is sub-second
	// digits and counts toward the instant (a ts one microsecond past
	// the future-skew allowance is beyond it, not "exactly at" it).
	// Integer nanoseconds, not float parsing: float64 loses precision
	// at epoch-seconds × microsecond scale.
	nsecDigits := frac
	if len(nsecDigits) > 9 {
		nsecDigits = nsecDigits[:9]
	} else {
		nsecDigits += strings.Repeat("0", 9-len(nsecDigits))
	}
	nsec, err := strconv.ParseInt(nsecDigits, 10, 64)
	if err != nil {
		return false
	}
	age := now.Sub(time.Unix(sec, nsec))
	if age < -slackTSMaxFutureSkew {
		return false
	}
	return age <= maxAge
}

// maybeDeliverReactionEvent is the sole handler for
// reaction_added/reaction_removed events (gp-by3). It either forwards
// the reaction to the conversation-bound session as a tagged
// notification or drops it; the caller returns either way. It never
// parses targets, never busy-marks, and never alias-dispatches.
func maybeDeliverReactionEvent(cfg config, env slackEventEnvelope) {
	var ev slackReactionEvent
	if err := json.Unmarshal(env.Event, &ev); err != nil {
		log.Printf("reaction: decode event: %v", err)
		return
	}
	// Only message reactions with a complete identity forward; legacy
	// file/file-comment reaction shapes carry no conversation to route.
	if ev.User == "" || ev.Reaction == "" || ev.Item.Type != "message" ||
		ev.Item.Channel == "" || ev.Item.TS == "" {
		return
	}
	// Rule 1: our own reactions (the busy affordance above all).
	if own := env.botUserID(); own != "" && ev.User == own {
		return
	}
	if cfg.companySelfBotUserID != "" && ev.User == cfg.companySelfBotUserID {
		return
	}
	// Rule 2: the fleet busy convention is mechanical, whoever reacts.
	if cfg.busyReaction != "" && ev.Reaction == cfg.busyReaction {
		return
	}
	// Rule 3: allowlisted fleet peers ack mechanically.
	if _, isPeer := cfg.peerBots.matchPeerByBotUserID(ev.User); isPeer {
		return
	}

	snippet, threadRoot := fetchReactionTargetContext(cfg, ev.Item.Channel, ev.Item.TS)

	inbound := externalInboundMessage{
		ProviderMessageID: ev.EventTS,
		Conversation: conversationRef{
			ScopeID:        cfg.cityName,
			Provider:       cfg.provider,
			AccountID:      cfg.accountID,
			ConversationID: ev.Item.Channel,
			// Reaction events carry no channel_type; the id-prefix
			// fallback classifies (C/G → room, D → dm).
			Kind: slackKindFromChannelType("", ev.Item.Channel),
		},
		// IsBot=true + the display prefix are load-bearing (rule 4):
		// gc records the transcript entry as bot-authored and the
		// intake helpers exclude it from anchoring on both markers.
		Actor: externalActor{
			ID:          ev.User,
			DisplayName: reactionActorPrefix + resolveUserDisplayName(cfg, ev.User),
			IsBot:       true,
		},
		Text:             formatReactionText(cfg, ev, snippet),
		ReplyToMessageID: threadRoot,
		DedupKey:         reactionDedupKey(env, ev),
		ReceivedAt:       time.Now().UTC(),
	}

	// No-wake buffering (gp-9e7 item 1): rooms and DMs alike, coalescing
	// enabled or not. ownTarget marks a reaction on one of the adapter's
	// own RECENT outbound posts — the founder-ack case allowed to ride
	// an ALREADY-armed coalesce window (it never arms one). Recency is
	// part of the definition (fix round 1c): a reaction on an
	// arbitrarily old bot post is ordinary traffic, not an ack of a
	// just-made post, and takes the plain side-buffer.
	ownTarget := ev.ItemUser != "" &&
		((env.botUserID() != "" && ev.ItemUser == env.botUserID()) ||
			(cfg.companySelfBotUserID != "" && ev.ItemUser == cfg.companySelfBotUserID)) &&
		slackTSWithin(ev.Item.TS, founderAckRecency, time.Now())
	if cfg.coalescer.admitReaction(ev.Item.Channel, pendingChannelInbound{inbound: inbound}, ownTarget) {
		log.Printf("reaction: buffered %s %q by %s chan=%s target_ts=%s own_target=%v (delivers with next real inbound)",
			ev.Type, ev.Reaction, ev.User, ev.Item.Channel, ev.Item.TS, ownTarget)
		return
	}
	// Nil coalescer (bare test configs): immediate forward fallback.
	if err := postInbound(cfg, inbound); err != nil {
		// Best-effort context: log and drop, dedup claim commits (a
		// Slack redelivery must not double-wake the session for an
		// emoji).
		log.Printf("reaction: forward failed chan=%s target_ts=%s (%v) — dropped (best-effort)",
			ev.Item.Channel, ev.Item.TS, err)
		return
	}
	log.Printf("reaction: delivered %s %q by %s chan=%s target_ts=%s",
		ev.Type, ev.Reaction, ev.User, ev.Item.Channel, ev.Item.TS)
}

// reactionDedupKey derives the gc dedup key. EventID is Slack's
// strictly-unique per-event id (stable across redeliveries); event_ts
// is the fallback for envelopes without one (test/synthetic shapes).
func reactionDedupKey(env slackEventEnvelope, ev slackReactionEvent) string {
	if env.EventID != "" {
		return "slack-reaction-" + env.EventID
	}
	return "slack-reaction-" + ev.EventTS
}

// fetchReactionTargetContext best-effort resolves the reacted-to
// message: a text snippet for orientation and the thread root for
// reminder threading. conversations.replies with ts=<target> returns
// the target itself for thread parents, plain messages, and (on
// current Slack behavior) mid-thread replies; the scan below only
// trusts an exact ts match, so a surface change degrades to no
// snippet, never a wrong one. Any failure returns ("", "").
func fetchReactionTargetContext(cfg config, channel, ts string) (snippet, threadRoot string) {
	if cfg.slackBotToken == "" {
		return "", ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), reactionTargetFetchTimeout)
	defer cancel()
	msgs, err := fetchThreadReplies(ctx, cfg.slackBotToken, channel, ts, reactionTargetFetchLimit)
	if err != nil {
		log.Printf("reaction: target lookup failed chan=%s ts=%s: %v (forwarding without snippet)", channel, ts, err)
		return "", ""
	}
	for _, m := range msgs {
		if m.TS != ts {
			continue
		}
		if m.ThreadTS != "" && m.ThreadTS != m.TS {
			threadRoot = m.ThreadTS
		}
		return truncateRunesClean(strings.TrimSpace(m.Text), maxReactionSnippetChars), threadRoot
	}
	return "", ""
}

// truncateRunesClean bounds s to max runes on a rune boundary,
// appending an ellipsis when anything was cut.
func truncateRunesClean(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	runes := []rune(s)
	return string(runes[:max]) + "…"
}

// formatReactionText renders the notification body. The Actor already
// names the reactor (gc's reminder header renders DisplayName), so the
// body opens with the verb — in a coalesced batch the line reads
// "[ts] reaction: Afik: reacted :+1: to …". Every interpolation is
// neutralized (cby.17/cby.33 discipline); gc's reminder formatter
// sanitizes the whole Text field on top.
func formatReactionText(cfg config, ev slackReactionEvent, snippet string) string {
	var b strings.Builder
	if ev.Type == "reaction_removed" {
		b.WriteString("removed their :")
		b.WriteString(neutralizeMarkupBoundaries(ev.Reaction))
		b.WriteString(": reaction from ")
	} else {
		b.WriteString("reacted :")
		b.WriteString(neutralizeMarkupBoundaries(ev.Reaction))
		b.WriteString(": to ")
	}
	if ev.ItemUser != "" {
		fmt.Fprintf(&b, "%s's message", neutralizeMarkupBoundaries(resolveUserDisplayName(cfg, ev.ItemUser)))
	} else {
		b.WriteString("a message")
	}
	fmt.Fprintf(&b, " at ts %s", neutralizeMarkupBoundaries(ev.Item.TS))
	if snippet != "" {
		fmt.Fprintf(&b, " (%q)", neutralizeMarkupBoundaries(rewriteSlackUserMentions(cfg, snippet)))
	}
	if ev.Type == "reaction_removed" {
		b.WriteString(" — earlier reaction withdrawn; no reply expected")
	} else {
		b.WriteString(" — ack/approval/emphasis signal; no reply expected")
	}
	return b.String()
}

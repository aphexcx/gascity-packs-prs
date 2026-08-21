package main

// --- channel-audience per-ts delivery claims (gp-ios) ------------------------
//
// A bot-mention arrives from Slack as TWO events — `message` +
// `app_mention`, same ts, DISTINCT event_ids — so the event_id dedup
// cannot collapse the pair, and both twins can race down the urgent
// channel-delivery path concurrently. gc's transcript append does
// dedup the RECORD by provider_message_id, but the member notification
// (the <system-reminder> the bound session actually reads) is built
// from each POST body and fired unconditionally: the session received
// the same message id twice in one turn, once bare and once wearing
// the thread-context preamble, whichever twin computed it first
// (pc_c920ff5fe90c, live 2026-08-20 06:44:02Z pair at 92ch/430ch).
//
// The claims cache serializes the CHANNEL-AUDIENCE delivery per
// (channel, ts), reusing the eventDedupCache two-state lifecycle:
//
//   - The first twin to begin() owns the delivery and MUST conclude it:
//     commit on a successful postInbound, forget on failure.
//   - A concurrent twin parks on the claim's done channel — bounded by
//     the owner's single gc forward — then re-begins: committed → its
//     channel copy is already delivered, skip; forgotten → take over.
//   - The coalesced-batch path claims each member the same way, so a
//     buffered twin and an urgent twin can never both deliver, in
//     either order, except in the documented fail-open case below.
//
// Fail open, never to loss: the batch path does NOT park on an
// in-flight claim (it holds the channel flush mutex; and if the urgent
// owner then failed, a dropped buffered copy would have no redelivery
// — Slack already got its 200). It keeps the member and accepts a
// possible duplicate in that narrow window instead.
//
// Same in-memory lifetime as deliveredIDs: a restart forgets claims,
// and the gc transcript's provider_message_id dedup plus the
// deliveredIDs seen-set remain the longer-horizon backstops.

// channelDeliveryClaimKey names one (channel, ts) channel-audience
// delivery in the claims cache. The channel qualifier keeps identical
// ts values in different channels (theoretically possible — Slack ts
// uniqueness is per channel) from colliding.
func channelDeliveryClaimKey(channel, ts string) string {
	return channel + "|" + ts
}

# Fresh same-model self-review

Reviewer: `/root/self_review`, a fresh subagent inheriting the implementation
session's model. Read-only review of the complete working delta against
`f507f7189184df2318011cfb4a6de8a82c5dc636`, including the new parity tests.
The reviewer did not edit the lane, run suites, or read private transcripts.
These are its returned verdicts and findings; paths below are repository-relative.

## Round 1 transcript

Review verdict: **CHANGES_REQUIRED**. Reviewed the current delta against
`f507f7189184df2318011cfb4a6de8a82c5dc636`, including the untracked
`peer_parity_test.go`.

1. **P2 — Recovery discards attachment metadata and can corrupt saved-file paths.**
   `slack-full/adapter/peer_bots.go:506` constructs the recovery item with only
   text and rendered Files; Attachments and trusted paths are lost. Recovery
   later prepends this prose to another inbound without restoring either capability.

   Reproduction: admit an allowlisted peer with a downloaded attachment,
   reject its immediate POST, then successfully deliver a later human or peer
   message. The recovered inbound lacks the original attachment. If its
   generated local path matches the redactor, the path is also changed because
   redactAttachmentText exempts only structured attachment URLs and trusted
   paths. Existing TestInboundRedactionWithheldPathsSurviveDeliveryAndSpool
   explicitly verifies that an untrusted prose-only path is redacted.

   Preserve attachment metadata or trusted file capabilities in queued entries
   and propagate them into the recovery envelope. Add an end-to-end failed-peer
   attachment test in both modes.

2. **P2 — A failed held recovery can permanently lose recovered peers.**
   `slack-full/adapter/inbound_coalescer.go:3416` records recovered peers as
   delivered, including when the receipt is held. However, the held-delivery
   dead-letter callback at line 3361 captures heldBatch, which contains only
   the original batch members—not the recovered peers prepended to env.Text.

   Reproduction: peer A's immediate POST fails; peer or batch B carries A;
   that POST returns held; receipt follow-up determines loss and its retry
   also fails. The dead letter contains B only. A is absent from the recovery
   FIFO and already marked delivered, so a same-timestamp redelivery is
   suppressed. The urgent human path retains the complete heldEnv and does
   not have this particular gap.

   Include recovered peers in the held delivery's durable failure record, or
   retain the complete submitted envelope. Add a held → lost → failed recovery
   test with a queued peer rider.

The replacement tests meaningfully cover whole UTF-8 bodies, both modes,
successful recovery through human/peer/batch paths, FIFO deferral, omission
counts, duplicate timestamps, equal author treatment, and newest-N pagination.
They do not cover either finding above; recovery fixtures also leave the
coalescer and receipt gate unset.

I found no additional actionable issue in allowlist/self admission, ordinary
reminder shape, author-neutral context shedding, newest-N readback, or the
measurement projection's documented limitations. This was a read-only code
and test review; I did not run suites or inspect private transcripts/live runtime data.

## Round 2 transcript

Verdict: **CHANGES_REQUIRED**. Both original findings are resolved, but
attachment propagation introduces one additional failure case.

**P2 — A rejected recovery attachment can repeatedly prevent unrelated messages
from delivering.** `peer_bots.go:314` adds recovered attachment metadata to the
successor envelope. On rejection, `inbound_coalescer.go:3407` restores the rider
unchanged, while the returned submittedDeliveryError identifies only the successor batch.

Reproduction:

1. GC rejects attachment-bearing requests with HTTP 400 but accepts text-only requests.
2. File-bearing peer A fails and enters recovery.
3. Attachment-free message B carries A's attachment and receives HTTP 400.
4. The rejection ladder sees B's original envelope with no attachments and
   dead-letters B. A remains unchanged in recovery and causes the same failure
   for subsequent messages.

Recovery needs to isolate rejected rider attachments—retaining their trusted
paths and a withholding notice—or retry the successor without riders before
charging it. Add a regression that drives this through the actual rejection ladder.

The original attachment/path-loss fix correctly preserves capabilities without
mutating the source slices. The held-loss fix retains each recovered peer's
original envelope for dead-lettering. Their new tests cover the reported
scenarios in both modes.

This was a focused read-only recheck; I did not independently rerun tests.

## Round 3 transcript

**CLEAN — all three findings are resolved.**

The revised path preserves attachment capabilities and trusted paths, retains
recovered peers in held-loss dead letters, and isolates rejected riders before
retrying the successor. Restored riders remain FIFO; deferred entries are
counted and are not marked delivered.

The new regression exercises the actual coalescer rejection path in both modes,
covering initial 503 and 400 responses, successful successor delivery, and
later whole-peer recovery with its path and withholding notice.

No new actionable findings in this focused read-only recheck. I inspected the
tests but did not independently rerun them; the reported 134-test pass comes
from the parent's validation.

## Implementation response

All three findings were fixed before the first commit. Queued entries retain
their original inbound; both delivery paths attach their file capabilities.
Held coalesced dead letters include each recovered entry. Permanent refusals
restore the riders and retry the successor alone with the deferred count;
the existing attachment-withholding helper retains paths and a notice.
Regression coverage is in `slack-full/adapter/injection_recovery_test.go`.

## Ordering follow-up transcript: first recheck

**CHANGES_REQUIRED — successful ordering is fixed; failed-flush context remains incorrect.**

**P2:** `peer_bots.go:605` still counts pending coalescer entries as already
delivered. flushAheadOf intentionally proceeds after failure or during
backoff, leaving those entries pending.

Reproduction: queue older human H; its flush returns 503; peer P fetches
thread history containing H and successfully posts. P omits H's text and
labels it “already delivered,” then advances the context watermark. H has
never reached the session.

Use deliveredIDs alone for peer-context collapse, and add a failed-flush
thread-context regression.

Lock ordering is safe: flushing occurs before holdDelivery, and blocking
mutex acquisition happens outside c.mu. Failed messages retain the existing
retry/dead-letter handling. The new both-mode test covers successful wire
ordering. No tests independently rerun.

## Ordering follow-up transcript: final recheck

**CLEAN — the ordering follow-up resolves the finding.**

Successful flushes deliver older messages before the peer. Failed or deferred
flushes leave those messages pending, and full-mode context now collapses
only IDs recorded as delivered. The new regression verifies the retained
quote, absence of a false delivery claim, and preserved retry ownership.

Lock ordering remains safe. No new actionable findings; tests were inspected
but not independently rerun.

## Ordering and pagination follow-up transcript

**CLEAN — ordering and pagination follow-ups are ready for the r2 gate.**

Newest-tail reads now use full API pages independently of the output limit,
retain only the newest N messages, and preserve response-cut fallback. The
updated tests verify both default 50 and limit 1 across 437 messages using
three 200-message requests.

The prior ordering and failed-flush fixes remain sound. No new actionable
findings. This was a read-only review; I did not rerun tests.

## Reply-anchor follow-up transcript

**CLEAN — the reply-anchor mismatch is resolved.**

Event selection, latest-transcript lookup, and explicit timestamp lookup now
admit peer inbounds consistently. Reaction notifications remain excluded
using the adapter's actual actor prefix.

The regression checks the real reply command across channels for both
threaded and top-level peers, ensuring it does not inherit an older human
thread. The shared helpers also cover react/upload anchoring. Updated docs
match this policy and identify the earlier inventory as historical.

No new actionable findings. Read-only review; tests were inspected, not
independently rerun.

# External Codex review response

Round 1 reviewed `5d665d86b110c384ed1c0229232dd6da5e3ed1f0` against the
fixed main base and returned BLOCK. Its transcript is `codex-review-r1.md`.

- Major: peer arrivals bypassed the human flush-ahead path and treated
  pending coalescer entries as delivered. The peer path now flushes before
  taking the delivery hold and only collapses confirmed delivered IDs.
  Tests cover successful human-before-peer wire order in both modes and a
  failed human flush followed by a peer reminder that retains the whole
  undelivered human quote without losing retry ownership.
- Minor: small output limits multiplied API requests while scanning to the
  newest thread messages. Thread reads now use the independent page ceiling,
  retaining only the newest N messages and preserving response-cut fallback.
  Tests cover default 50 and limit 1 over 437 messages with three requests.

The same-model reviewer rechecked both changes and returned CLEAN. The final
external gate and complete final check summaries accompany the handoff.

Round 2 reviewed `43e982a75af86f24553c1510fa98817178f14c40` and returned
BLOCK on inconsistent reply anchoring: a plain-label peer event could select
a conversation while transcript lookup still skipped that peer and inherited
an unrelated human thread. Its transcript is `codex-review-r2.md`.

The implementation removes the remaining bot exclusions from default and
explicit-turn anchor lookup. This deliberately supersedes the initial SETUP
claim that automatic anchors prefer humans: the bead requires peer posts to
be ordinary posts and leaves response policy to each city's prompt. Events
and transcripts now select the same latest inbound, whether human or peer.
Synthetic reaction notifications stay excluded because their event timestamps
are not postable message timestamps. Tests exercise human C1 → peer C2 and
actual default replies under a peer's thread or at channel level, plus exact
peer-turn lookup. The pre-design inventory remains unchanged as historical
evidence; the updated note explicitly records the policy correction.

# gp-byp8: the lane's fresh self-review r1, and its disposition

**The reviewer.** An Opus subagent, given the round delta `e39c0b90..17a54f6e`, the gate r3 record, PR 76's body and the README, but not the lane's reasoning. It ran its reproductions as tests in its own detached worktree (`review.test.ts`, kept at `/private/tmp/gp-byp8-review/tree/whatsapp/gateway/test/`). It also ran them at e39c0b90 and ba82ab46.

**The verdict.** BLOCK: 2 CRITICAL, 1 MAJOR, 1 MINOR, 2 NIT. The lane re-ran every reproduction at d520e098 (after the codex self-review r1 fix): the MAJOR passed there, and six failed.

| Finding | Reproduction | Disposition |
|---|---|---|
| **CRITICAL 1.** A pairing Meta delivers only after A's notice names A's previous user_id. `pairingOpens` dropped it, so A's T3 image, delivered after both, reached W with its media (both notice shapes). The same root cause hit an earlier pairing landing after the notice ended A's open span (R1c). | R1 x2 and R1c fail at 17a54f6e and d520e098 | **Fixed (3b1ba734).** `pairingHistory` keeps the pairing as A's span of P up to that change. It merges into the row the change ended and moves its start back, never before A's bind. Tests: "a pairing Meta delivers only after A's notice (which names P / no old number) ...", "a pairing older than A's span, landing only after A's notice ended it ...", "a late pairing stamped before the owner bound keeps nothing from before the bind". |
| **CRITICAL 2** (older than this round; this round's README claimed otherwise). Once an operator unbound A, rule 3 gave C A's messages naming P beside A's user_id, and A's notice. C's row took A's user_id or A's new ids, so A's later messages reached W. | R2 and R4 fail at 17a54f6e, d520e098, e39c0b90 and ba82ab46 | **Fixed (3b1ba734).** When the current holder is a phone-only row reached by rule 3, each binding's span of the number that holds the stamp is a second holder: the message goes to nobody, as a conflict. That only refuses: a span never routes a message naming a user_id. The fill, the change's span end, the alias insert and the re-key find the owner by `routeKey`, so a phone-only row in doubt takes nothing. Tests: "once an operator unbinds A, A's image naming P beside A's user_id / A's notice, then A's messages under its new ids ...", "the rule-3 doubt only refuses ...". |
| **MAJOR.** A span reopened before an earlier end collided on the primary key, so every redelivery of the payload failed. | R3 x2 fail at 17a54f6e; pass at d520e098 | **Fixed by d520e098** (the codex self-review r1's fix: a new span starts after the last end, and history never reopens). In 3b1ba734, every end also merges into a row of the binding's already ending at that stamp (`UPDATE OR REPLACE` with the earlier start), so no delivery order can fail a payload on the key. Tests: the two merge tests and the replay tests. |
| **MINOR.** Unbinding A rather than C leaves the conflict until the next cron, so C's own messages go to nobody until then. | R5 | **Named, with a remedy (3b1ba734).** The README's monitoring line says unbinding the phone-only owner settles it at once, and unbinding the other owner settles it at the next cron. "Unbind an owner" gives the statement that ends the owner's spans at once. The code keeps the span: until the unbind time is known, a span ending earlier would hand A's pending retries to W. |
| **NIT.** The README said a span starts at "the number change ... when that change shared no number", but the code uses any `wa_since`. | read | **Fixed:** the README now says "where the owner came to its current number (`wa_since`)". |
| **NIT.** A redelivery of a message a city already holds was counted as unrouted (and as a conflict). | read | **Fixed:** not counted (test "Meta's redelivery of a message a city already holds is not counted ..."). |

**What the reviewer checked and found sound.**
- The gate's in-order sequence and the round's table.
- `holders`, `routeKey` and the counter statement.
- Bound-parameter use.
- What ends a span.
- Strangers and binds, media, and the cap note.

**`probe-rule3-unbound-owner.test.ts.txt`.** The lane's own probe of CRITICAL 2's first half. It passed at e39c0b90 and 17a54f6e (W got the retry and C took A's user_id), so it reproduced the limit. At 3b1ba734 its two assertions fail, because the limit is fixed.

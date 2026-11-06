# Fresh Opus read r1 on the round delta af7e2d9b..dc376d24 (gp-g02t self-review)

One Opus 5.5 subagent, briefed with the bead's scope and the code, not the lane's conclusions; its
own copy of the tree under /private/tmp/gp-g02t-review (git archive at dc376d24). Its report as it
returned it (11/6 ~12:01Z), reformatted only.

**Verdict: BLOCK, 1 CRITICAL and 2 MAJOR.** One root cause: a `conversation_id` that is a phone
number is not a unique key at the gateway once its owner is gone, or once another same-city owner
shows that number. Everything else on the inbound side is consistent: every adapter path reads
`e.conversationId` from the received line, and the PR 74 journal and progress records are read as
written.

Test runs in its copy: adapter suite 188/188, `test/conversation.test.js` 6/6, pack pytest 45/45.
Scratch files in its copy: `test/zz-review-keeper-unbound.test.js`, `test/zz-review-keeper-bsuid.test.js`, `sql/r.db`.

## Findings

1. **CRITICAL: a number-form conversation outlives its owner, and sends to it reach a different owner.**
   `outbound.js:338` and `:453` send `to: <conversation_id>`; `keeper.js:710` raises events for every
   keeper row and never drops a row the gateway stops listing; the gateway's send
   (`gateway.ts:1026-1033`) accepts `to` as a `conversation_id`, a `user_id`, or a number whose
   newest pairing is with an owner. Owner A keyed by P; P passes to B (same city, keyed `B_USER`,
   P now points to B); the operator unbinds A; A's keeper row P (consent ON) keeps raising
   "[window-keeper] P: daily update due"; the mayor's `--update --to P` resolves to B alone.
   Reproduced: the gateway SQL in sqlite (`to=P` names `5511900000001,US.B1` before the unbind,
   `US.B1` after); a keeper scratch test (an update raised for a conversation the gateway no longer
   lists). Not introduced by this delta (af7e2d9b sends the same `to`), but it defeats the round's
   outcome. Fix sketch: resolve `conversation_id` to `user_id` from a fresh `GET /v1/window`, send to
   it, refuse when the window doesn't list it, fall back to `to: conversation_id` only when the entry
   has no `user_id` field (PR 73); the keeper raises nothing for rows missing from the last good read.
2. **MAJOR: while A stays bound, A's own conversation can never get a reply after a recycle.**
   `outbound.js:255-268`; `gate.test.js:545-562` asserts the refusal. `to=P` names A (key) and B
   (number): every reply and update to A is a permanent 409. Since gp-g02t the conversation means the
   keyed owner, so the old reason for refusing no longer holds. Fix: finding 1's resolution.
3. **MAJOR: when the old owner is unbound before the new one binds, both share one gc conversation.**
   `gateway.ts:715-719`: B's key is P whenever no row holds P, so B lands in `whatsapp/P` with A's
   transcript, media directory, keeper row, ledger row and dead-letter file. Contradicts
   `whatsapp/README.md:80-83` and the CHANGELOG. The gateway's key rule: a design question.
4. **MINOR: the keeper matches a conversation id by its digits only** (`keeper.js:304-311`, `:385`).
   A BSUID allows letters (`gateway/src/meta.ts:20`). Reproduced: `consent --off US.Zz7q` matched
   `US.Ab7c`; a BSUID with no digits refused. Fix: exact id first; digits only for phone-shaped input.
5. **MINOR: the new tests don't pin the recycle claim**: shape 3 tests only B's reply; the fake
   gateway models no number match and no 409.
6. **MINOR: the verbs and a running old adapter disagree** after a pack roll (consent, hours,
   clear-gap send only `conversation_id`; resolve/status print `...null`). Fix: send both fields,
   read `conversation_last4 // wa_id_last4`.
7. **NOTE: rolling back to the PR 74 adapter is unsafe** once new lines exist (they carry no `wa_id`).
8. **NOTE: docs now slightly untrue**: "a user id for someone who joined without showing one"
   (a recycled-number joiner gets one too); phone wording in `resolve/help.md:20`, `invite.sh:7`,
   `status/help.md:12`; dead-letter names strip the dot; clear-gap needs the exact id.
9. **NOTE: `publish --to` a number the owner shows** records the CLI transcript in a separate
   conversation and skips the ledger's window-open guard.

VERDICT: BLOCK

## What the lane did (the mayor's answer ci-wisp-d8ny5e3, 12:05Z: (A) YES, M3 in the gateway)

- 1, 2, 5, 9: the recipient from one fresh `GET /v1/window` per publish or update (outbound.js THE
  RECIPIENT); the keeper raises nothing for a row the last good read did not list, kept in
  keeper.json (`window_ids`). RED: review-r1/red-recipient-at-dc376d24.txt (C1, M2, `--to` a number),
  review-r1/red-keeper-unlisted-at-dc376d24.txt.
- 3: the gateway keys a new binding by its user id (gateway.ts bind); PR 73 rows keep their number
  until the re-invite. RED: review-r1/red-m3-bind-key-at-dc376d24.txt.
- 4: the keeper matches an id whole, a typed number only against a number's conversation. RED:
  review-r1/red-keeper-verbs-at-dc376d24.txt (codex r1's MAJOR 1 too).
- 6: the verbs send both names and read either answer key (pytest).
- 7: stated in the READY as a limit (the adapter serves both gateways, so it rolls forward only).
- 8: the docs and help fixed; dead-letter names keep the period (journal.test.js).

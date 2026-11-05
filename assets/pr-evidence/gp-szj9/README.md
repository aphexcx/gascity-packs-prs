# PR 74 round 3 (gp-szj9): the gateway key binds the recipient and the whole message; PR 76's 409 ambiguous_recipient is refused, never resent

The round, on 24d75c41:

| Commit | What |
| --- | --- |
| `e5a7fda6` | The Opus read r2's MAJOR 1 (`/Users/tailor512/city/assets/ops/codex-gates/gp-9hf1-pr74-opus-read-r2-24d75c41.md`, BLOCK 0/1/0/5; the codex gate r2 on the same delta was CLEAN): the gateway key binds the recipient and the whole message. |
| `be93583a` | The bead's ADDENDUM 1 as first written: on PR 76's 409 `ambiguous_recipient`, resend once to the `user_id` from `GET /v1/window`. |
| `95c9c1b9` | Evidence for those two. Codex self-review r1 on 24d75c41..95c9c1b9: CLEAN. |
| `937c56d4` | The round's Opus self-review r1 (BLOCK 0/1/0/4) proved be93583a's resend misdelivers at this head. The mayor ruled (ci-wisp-f53rmv3, 00:34Z 11/5): refuse, `gateway_refused` as before, the ambiguity named, no resend. The resend is gone. |
| this commit | Evidence for the final head. |

## 1. The key (`e5a7fda6`)

`whatsapp/adapter/src/outbound.js`:

- `textChunkKey(key, to, text, i)` = `text:<sha256 of JSON [key, to, the whole text]>:<i>`.
- `templateKey(key, to, template)` = `template:<sha256 of JSON [key, to, name, language, params]>`, built from the very template object sent.

Round 2's keys were `text:<sha256 [K, chunk]>:<i>` and `template:<sha256 [K, text]>`. Once the adapter's progress store forgot a caller key (7 days or 1,024 keys), a reuse it would have refused (409) went to the gateway under a key the gateway still remembered (30 days, keyed by city and key alone). It was reported delivered with another message's id.

The new keys cover the fingerprint the store checks (phone, text, verb), so the store's shorter memory is harmless. A key the store forgot, reused for another owner or another message, is a new gateway key. The same key, recipient and message is still the gateway's first answer (the REPEATED KEY rule). A template key is 73 characters; a text chunk's is 70 plus the index's digits. The gateway's bound is 1 to 200.

## 2. The 409 (`be93583a`, then `937c56d4`)

**The contract.** PR 76 (gp-0g2s, ba82ab46) answers a send whose `to` names two owners of the city with `409 {error: "ambiguous_recipient", hint: "send to the owner's current id"}`. The two owners are one owner's `conversation_id` (kept from a phone number it no longer holds) and another owner's current `wa_id`: a recycled number. Nothing is sent and no key is spent.

**What be93583a did.** It resent to the `user_id` of the window entry whose `conversation_id` is the number.

**Why that was wrong at this head.** This adapter files a delivery under its `wa_id` (render.js:118-122; the `conversation_id` keying is the Opus read r2's NOTE 3, deferred). After a recycle within one city, the NEW owner B's messages sit in the conversation keyed by the number, and the number's window entry is the OLD owner A's. So the resend handed B's reply and update to A. `self-review-opus-r1.md` holds the review with its probe.

**What 937c56d4 does.** `failure()` answers the 409 as the permanent `gateway_refused`, with `metadata.gateway_error ambiguous_recipient` and a hint that names the ambiguity and quotes the gateway's hint. There is no window read and no resend. The resend to the owner's `user_id` waits for the `conversation_id` keying bead (the mayor files it).

## The gateway's facts the fixes rest on

At 392f9c09 (PR 73, merged):
- `whatsapp/gateway/src/gateway.ts:56` `const SENDS_KEEP_SECONDS = 30 * DAY;`
- `gateway.ts:746` `boundedString(body.idempotency_key, 1, 200)`
- `gateway.ts:760-763`: the repeat lookup `SELECT wamid, state, created_at FROM sends WHERE city_id = ? AND idempotency_key = ?`. The recipient is not part of a key's identity.
- `migrations/0001_init.sql` `sends`: `PRIMARY KEY (city_id, idempotency_key)`.
- No `ambiguous_recipient` anywhere.

At ba82ab46 (PR 76, DRAFT):
- `gateway.ts:1032-1057`: `to` matches `conversation_id`, `wa_id` or `user_id` (LIMIT 2), else 403. The repeated key is answered BEFORE the ambiguity check. Two rows answer the 409.
- `gateway.ts:1164-1176`: each window carries `conversation_id` and `user_id`.
- `gateway.ts:697-698`: a bind on a number that is already a `conversation_id` keys the new owner by its `user_id`.
- `gateway.ts:737`: a message delivery carries `wa_id` = the sender's number.
- `whatsapp/gateway/README.md` rule 7.

## RED first

The tests are in `whatsapp/adapter/test/gate.test.js`. The fake gateway (`test/fakes.js`) gains PR 76's surface: `userIds` on a window entry, a send naming a user_id, and `ambiguous` numbers answering the 409 after the repeated-key check, as at ba82ab46. Nothing it answered before changes.

**"Opus read r2 MAJOR 1".** Both tests forget the key by the store's own 7-day age: the clock moves past the TTL, an unrelated update is the store's next write, and the test checks the progress file no longer holds the key.
- (a) The same text and the same update under K, first to owner A, then to owner B after the store forgot K and U. B's copies must reach Meta under B's own ids. Inside the store's memory both are refused 409.
- (b) A two-chunk text under K, then another with the same first 4,096 characters under K after the store forgot it. Every chunk must go out.

**"ADDENDUM 1".**
- (c) A text and an update to a number naming two owners. Each is refused once (`gateway_refused`, `gateway_error ambiguous_recipient`, the hint naming the ambiguity and quoting the gateway's), with never a second send, no window read, and nothing reaching Meta.
- (d) The self-review's probe, end to end through the inbound engine. B's message (wa_id the number, user_id and conversation_id B's) is filed by wa_id; the mayor's reply and update in that conversation never reach A's user_id.

`make_red.py` rebuilds three RED trees: the `git archive` of each commit, plus the head's `test/gate.test.js` and `test/fakes.js`.

    python3 -I assets/pr-evidence/gp-szj9/make_red.py /tmp/gp-szj9/red3
    cd /tmp/gp-szj9/red3/<24d75c41|e5a7fda6|be93583a>/whatsapp/adapter
    node --test --test-reporter=spec test/gate.test.js

Every failure below is an AssertionError; none is a harness error.

- `node-gate-RED-24d75c41.txt`: 17 tests, 4 fail.
  - (a): `actual: { text: [A], update: [A] }`.
  - (b): `actual: ['<4096 x a>', 'FIRST TAIL', 'SECOND TAIL']`.
  - (c) and (d): a bare `gateway_refused`, the ambiguity unnamed (`error: undefined`). The refusal itself and "never a second send" already held there.
- `node-gate-RED-e5a7fda6.txt`: 17 tests, 2 fail: (c) and (d), the ambiguity unnamed.
- `node-gate-RED-be93583a.txt`: 17 tests, 2 fail.
  - (c): `delivered: true`, `tos: [A, US.ownerA2, US.ownerA2, A, US.ownerA2]`, two window reads: the resend.
  - (d): Meta asked to send `[["text","US.ownerA2"],["template","US.ownerA2"]]`: B's reply and update to A.

The round-2 tests (the gate r1's MAJOR 3 sequence, its reverse, NOTE 5's reuse after eviction) pass in all three trees and at the head.

## GREEN

Node v24.21.0 through `/Users/tailor512/city/.gc/shims/toolchain/pnpm exec node`, `--test-concurrency=2 'test/*.test.js'`:

- `node-GREEN.txt`, the tree committed as 937c56d4: **178/178** at load 6.3, 83.7 s. That is round 2's 173, plus (a), (b), (c), (d) and the key table in outbound.test.js.
- `node-GREEN-e5a7fda6.txt`, the tree committed as e5a7fda6: 176/176.
- `pytest-GREEN.txt`: `env -u GC_TEMPLATE uv run --with pytest --with pyyaml --with jsonschema python3 -m pytest whatsapp/tests -q -p no:cacheprovider`: **44 passed**.

## The wait-timeout flake (NOTE 4, pre-existing)

Two full runs of the same tree at load 11 to 19 each had wait timeouts, never a wrong outcome:

- `node-run4-937c56d4-6b-timeout.txt`: recovery (6b), "timed out waiting for the ack of delivery 1".
- `node-run5-937c56d4-pipeline-timeouts.txt`: pipeline.test.js "marked read" and "a 404 retries".

They are the class the Opus read r2's NOTE 4 names: a fixed clock pump, then an un-pumped wait on a real child. A diagnostic copy that dumps the child's state on a timeout showed gc with three of the four POSTs and the clock stopped at the pump's end. The loaded child answered late, and its next repost fell past the pump.

Comparisons with a pristine `git archive 24d75c41`:

- (6b) alone, three runs each, alternating, at load ~10: 24d75c41 failed 1 of 3, the head 0 of 3.
- `flake-ab-pipeline.txt`: pipeline.test.js, four interleaved rounds at load 6 to 7.5. Pristine 24d75c41 failed 0 of 4 and the head worktree 0 of 4. A diagnostic copy of the head, with the same code plus the dump, failed 1 of 4.

Earlier sequential runs had the head copy failing 3 of 3 while 24d75c41 passed 3 of 3 as the load fell, so only the interleaved rounds count. The round changes nothing on the inbound path: the inbound repost ladder never calls the changed outbound functions, and the fakes' additions are inert without `userIds` or `ambiguous`.

## Reviews

- `self-review-codex-r1-95c9c1b9.txt`: the lane's codex self-review through the hub (`codex-gate.sh review`, STANDARD from the bead) on 24d75c41..95c9c1b9. **CLEAN.** It ran before the fold and missed the misdelivery.
- `self-review-opus-r1.md`: a read-only Opus subagent on 24d75c41..95c9c1b9. **BLOCK**, 0/1/0/4.
  - MAJOR 1, the misdelivery: fixed in 937c56d4 on the mayor's word.
  - NOTE 1, the template key binds the language: kept, as the bead's scope names it; a retry after a language change is a new key.
  - NOTE 2, in-flight publishes under round-2 keys: moot, since PR 74 never ran against a real gateway.
  - NOTE 3, `cAt >= 0`: folded.
  - NOTE 4, the ambiguity named: folded into the refusal.
- `self-review-codex-r2.txt`: the codex self-review on the whole round delta, 24d75c41..the evidence commit before it.

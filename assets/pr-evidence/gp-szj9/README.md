# PR 74 round 3 (gp-szj9): the gateway key binds the recipient and the whole message; PR 76's 409 ambiguous_recipient

Two changes on 24d75c41, each RED first:

1. `e5a7fda6`: the Opus read r2's MAJOR 1
   (`/Users/tailor512/city/assets/ops/codex-gates/gp-9hf1-pr74-opus-read-r2-24d75c41.md`,
   BLOCK 0/1/0/5; the codex gate r2 on the same delta was CLEAN). Round 2's
   gateway keys `text:<sha256 [K, chunk]>:<i>` and `template:<sha256 [K, text]>`
   bound neither the recipient nor the whole message. Once the adapter's progress
   store forgot a caller key (7 days or 1,024 keys), a reuse it would have refused
   (409) went to the gateway as a key the gateway still remembered (30 days, keyed
   by city and key alone) and was reported delivered with another message's id.
2. `be93583a`: the bead's ADDENDUM 1, PR 76's `409 ambiguous_recipient`
   (gp-0g2s at ba82ab46, the mayor's rule R1): resend once to the owner's
   `user_id` from `GET /v1/window`.

## The fixes

- `whatsapp/adapter/src/outbound.js`: `textChunkKey(key, to, text, i)` =
  `text:<sha256 of JSON [key, to, the whole text]>:<i>`; `templateKey(key, to,
  template)` = `template:<sha256 of JSON [key, to, name, language, params]>`,
  built from the very template object sent. Both cover the fingerprint the
  progress store checks (phone, text, verb), so its shorter memory is harmless: a
  key it forgot, reused for another owner or another message, is a new gateway
  key; the same key, recipient and message is still the gateway's first answer
  (the REPEATED KEY rule). A template key is 73 characters, a text chunk's 70
  plus the index's digits (the gateway's bound: 1 to 200).
- `sendMessage(route, body)`: one path for a text chunk and a template. On 409
  `ambiguous_recipient` it reads `GET /v1/window` once, takes the `user_id` of the
  entry whose `conversation_id` is the number sent to, and resends the message
  there under the same gateway key; a text's later chunks go to the same id. A
  second 409, or no `user_id` (PR 73's gateway sends none), is `gateway_refused`
  as before; a failed window read is the failure (transient on a 5xx).

## The gateway's facts the fixes rest on

At 392f9c09 (PR 73, merged):

- `whatsapp/gateway/src/gateway.ts:56` `const SENDS_KEEP_SECONDS = 30 * DAY;`
- `gateway.ts:746` `boundedString(body.idempotency_key, 1, 200)`
- `gateway.ts:760-763` the repeat lookup `SELECT wamid, state, created_at FROM
  sends WHERE city_id = ? AND idempotency_key = ?`: the recipient is not part of
  a key's identity. `migrations/0001_init.sql` `sends`: `PRIMARY KEY (city_id,
  idempotency_key)`.
- No `ambiguous_recipient` anywhere; `GET /v1/window` selects `wa_id,
  last_inbound_at` (no `user_id`).

At ba82ab46 (PR 76, gp-0g2s round 2, DRAFT):

- `gateway.ts:1032-1057`: `to` matches `conversation_id`, `wa_id` or `user_id`
  (LIMIT 2); 403 when none; the repeated key answered BEFORE the ambiguity
  check; two rows answer `409 {error: "ambiguous_recipient", hint: "send to the
  owner's current id"}`, nothing sent, no key spent.
- `gateway.ts:1164-1176`: each window carries `conversation_id` and `user_id`.
- `whatsapp/gateway/README.md` rule 7: "The box resends to the owner's current
  id, which is the `user_id` of the owner's entry in `GET /v1/window`."

## RED first

The key tests (gate.test.js, "Opus read r2 MAJOR 1") forget the key by the
store's own 7-day age: the clock moves past the TTL, an unrelated update is the
store's next write, and the progress file is checked to no longer hold the key.
The fake gateway answers a repeated key from a map keyed by the key alone, as
gateway.ts does.

- (a) the same text and the same update under K to owner A, then to owner B
  after the store forgot K and U (inside its memory both are refused 409): B's
  copies must reach Meta under B's own ids.
- (b) a two-chunk text under K, then another two-chunk text with the same first
  4,096 characters under K after the store forgot it: every chunk must go out.

The ADDENDUM 1 tests drive the fake gateway's PR 76 surface (`test/fakes.js`:
`userIds` on a window entry, a send naming a user_id, `ambiguous` numbers
answering the 409 after the repeated-key check as at ba82ab46, `windowFailures`):

- (c) a two-chunk text: 409 at the number, one window read, both chunks to the
  user_id, the resend under the same gateway key;
- (d) an update: resent to the user_id;
- (e) no `user_id` on the entry, a second 409 on a text, a second 409 on an
  update: `gateway_refused` (409), one resend at most, nothing sent;
- (f) a failed window read: transient; the retry under the same key delivers.

`make_red.py` rebuilds both RED trees (`git archive` of the commit plus the
round's `test/gate.test.js` and `test/fakes.js`; the fakes' additions change
nothing they answered before):

    python3 -I assets/pr-evidence/gp-szj9/make_red.py /tmp/gp-szj9/red2
    cd /tmp/gp-szj9/red2/<24d75c41|e5a7fda6>/whatsapp/adapter
    node --test --test-reporter=spec test/gate.test.js

- `node-gate-RED-24d75c41.txt`: 19 tests, 13 pass, 6 fail: (a), (b) and the four
  ADDENDUM tests, each an AssertionError at its behavioural assertion: (a)
  `actual: { text: [A], update: [A] }`; (b) `actual: ['<4096 x a>', 'FIRST
  TAIL', 'SECOND TAIL']`; (c)-(f) `windowReads: 0`, every 409 `gateway_refused`
  (permanent, even the window-read case).
- `node-gate-RED-e5a7fda6.txt`: 19 tests, 15 pass, 4 fail: the four ADDENDUM
  tests only.

The round-2 tests (the gate r1's MAJOR 3 sequence, its reverse, the
self-review NOTE 5 reuse after eviction) pass in both trees and at the head.

## GREEN

Node v24.21.0 through `/Users/tailor512/city/.gc/shims/toolchain/pnpm exec node`,
`--test-concurrency=2 'test/*.test.js'`, host load average 8 to 13:

- `node-GREEN.txt`, the tree committed as be93583a: **180/180** (round 2's 173,
  plus (a), (b), the key table in outbound.test.js and (c)-(f)), 82.8 s.
- `node-GREEN-e5a7fda6.txt`, the tree committed as e5a7fda6: 176/176.
- `pytest-GREEN.txt`: `env -u GC_TEMPLATE uv run --with pytest --with pyyaml
  --with jsonschema python3 -m pytest whatsapp/tests -q -p no:cacheprovider`:
  **44 passed**.

Neither of this round's two full node runs had a wait timeout.

## Self-review

`self-review-codex-r1.txt`: the lane's codex self-review through the hub
(`codex-gate.sh review`, class STANDARD from the bead) on 24d75c41..the round
head.

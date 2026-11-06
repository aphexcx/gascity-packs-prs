# gp-g02t: PR 76 round 2, the box adapter on the gateway's stable conversation_id

The bead: the mayor's gate r4 MAJOR 1 and the Opus read r1 NOTE 5 at af7e2d9b. The box adapter keyed
every owner by `wa_id`, which PR 76's gateway leaves NULL for an owner known by user id alone,
changes at a number change, and shares between two owners after a recycled number. Round 2 on the
same PR, on the mayor's ruling.

## RED first, then GREEN

`make_red.py` builds the adapter at af7e2d9b with the head's `test/conversation.test.js` and
`test/fakes.js` laid over it, and the adapter at the head, with `git archive`, and runs the file in
each. `python3 -I assets/pr-evidence/gp-g02t/make_red.py OUT [HEAD]` from the repository root.

- `red/conversation-at-af7e2d9b-make_red-e3f09f38.txt`: 11 cases, 10 fail at af7e2d9b. The one that
  passes is the named guard (a PR 73 row is refused by the gateway's own 403 there too). The three
  shapes:
  - a NULL wa_id (a message and a join): left unacknowledged;
  - a wa_id changed within one conversation_id: two chains, filed under the new number;
  - a recycled number: B filed under the number A was bound by.
  The rest:
  - the PR 74 journal line;
  - the contract boundary;
  - the recipient C1 (a reply reached the owner who shows the unbound owner's number), M2 (refused
    409), and `--to` a number;
  - the keeper verbs.
- `green/conversation-at-e3f09f38.txt`: 11/11 at the head.
- `red/conversation-at-af7e2d9b-make_red.txt` and `red/conversation-at-f19daa2b-first-run.txt`: the
  first six cases at the RED commit f19daa2b, 6/6 RED.
- `review-r1/red-*.txt`: the self-review r1 findings reproduced at dc376d24 before their fixes:
  - `red-recipient-*`: C1, M2 and `--to` a number;
  - `red-keeper-verbs-*`: consent for US.ownerB1 answered `erA1`;
  - `red-keeper-unlisted-*`: two updates raised for an unlisted conversation;
  - `red-m3-bind-key-*`: B and C keyed by their numbers.
- `minor1/`: the gate r4 MINOR 2, the Worker-entry test.
  - `scratch-minor1-red*.test.ts` are the head's test with af7e2d9b's seed (a row with no user id),
    never committed under test/.
  - `red-old-seed.txt`: 0 deliveries (the stranger path).
  - `red-old-seed-fetch.txt`: one call to https://graph.facebook.com/v26.0/106540352242922/messages.
  - At the head the owner is seeded as the bind writes it, the delivery is asserted, and the global
    fetch is mocked and asserted untouched.

## The runs

At fddfb320 (the codex r2 fold; 945dd60e after it changes only docs and one verb's labels; pytest
46/46 there):
- `green/gateway-suite-at-fddfb320.txt`: 229/229. `green/gateway-typecheck-at-fddfb320.txt`: exit
  0. `green/gateway-dry-run-at-fddfb320.txt`: exit 0, 54.90 KiB.
- `green/adapter-suite-at-fddfb320.txt`: 194/195. The one failure is pipeline "a partial receipt
  reposts three times ...", a wait timeout. It passed 3 of 3 alone.

At e3f09f38:

- `green/gateway-suite-at-e3f09f38.txt`: 229/229 (bsuid-orders 62, bsuid 68, gateway 69, meta 30).
  The eight cases are unchanged in outcome. The 103 workerd ".text() on an HTTP body" warnings are
  the harness reading fake image bytes, test side.
- `green/gateway-typecheck-at-e3f09f38.txt`: exit 0. `green/gateway-dry-run-at-e3f09f38.txt`: exit
  0, 54.90 KiB.
- `green/pack-pytest-at-e3f09f38.txt`: 46/46.
- `green/adapter-suite-at-e3f09f38-run4-clean.txt`: 195/195.
- `green/adapter-suite-at-e3f09f38.txt`: an earlier run at the same head, 194/195. The one failure is
  review.test.js "M2: an inbox page computed before the ack ...", a wait timeout at host load around
  11. It passed 3 of 3 alone.
  - The two earlier full runs each had exactly one such timeout, a different test each time:
    `adapter-suite-at-dc376d24-run1.txt` (restore (11i)) and `adapter-suite-mid-fold-run2.txt` (the
    keeper-verbs wait, since pumped).
  - Each passes alone. This is the fixed-clock wait class recorded on PR 74 (gp-wf4e).
- `red/adapter-suite-at-af7e2d9b.txt`: the base, 178/178.

## The self-review

- `review-r1/codex-r1-dc376d24.txt`: codex through the hub, BLOCK: 1 MAJOR (the keeper's digit
  match) and 4 MINOR.
- `review-r1/opus-fresh-read-r1-dc376d24.md`: a fresh Opus reader, BLOCK: 1 CRITICAL and 2 MAJOR, one
  root (a number-form conversation_id is no unique send key). What the lane did is at its foot.
- `review-r1/design-question-mail.txt`: the question to the mayor. The answer (ci-wisp-d8ny5e3,
  12:05Z 11/6) was (A) YES (the recipient from GET /v1/window), and M3 fixed in the gateway this round.
- `review-r2/codex-r2-e3f09f38.txt`: codex on the whole round delta af7e2d9b..e3f09f38, BLOCK: 1
  MAJOR and 3 MINOR. The MAJOR: after M3, bsuid-orders' "C unchanged" checks compared null with
  null. All four were folded in fddfb320.
- `review-r3/codex-r3-fddfb320.txt`: codex on the whole round delta af7e2d9b..fddfb320, VERDICT
  CLEAN, with 3 MINOR: the clear-gap label, the skill's "in any form", and consent's help cut short.
  They were folded in 945dd60e.
- Each `codex-r*-prompt.md` is the prompt that read ran on (`codex-gate.sh exec`, the hub route,
  gpt-6-astra).

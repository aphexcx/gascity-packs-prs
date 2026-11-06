# gp-wf4e: PR 74 round 2, the codex gate r1's findings

The gate of record: /Users/tailor512/city/assets/ops/codex-gates/gp-9hf1-pr74-r1-ba9155312.txt
(STANDARD, gpt-6-astra through the hub, bc2fd143..ba915531, 22:00-22:08Z
Wed 11/4): BLOCK, MAJOR inbound.js:420, MAJOR inbound.js:284, MAJOR
outbound.js:398, MINOR inbound.js:197. The Fable read r1 at the same head
(gp-9hf1-pr74-fable-read-r1-ba915531.md): CLEAN, 0/0/1/17; its MINOR 1 is
taken here too (WINDOW_TYPES, read in takeDelivery, a function this round
rewrote).

Host: citadel (macOS), node v24.21.0 through the toolchain pnpm wrapper, no
install (the adapter has no dependency); pytest through
`env -u GC_TEMPLATE uv run --with pytest --with pyyaml --with jsonschema
python3 -m pytest` (the host's bare python3 has no pytest). Other city
agents kept the load average between 7 and 79 through the round.

## RED first

- `node-gate-RED.txt`: test/gate.test.js written FIRST and run against
  ba915531's code before any source changed: 10 tests, 10 fail, each at the
  gate's own consequence:
  - MAJOR 1 (held head read; failed head read): B1's second POST plain,
    `never an unmarked duplicate: "delivered, the answer lost"`;
  - MAJOR 2: no classifier (`'undefined' !== 'function'`); the fold reads a
    received line's recovery as `received`, not `posted`; the crash +
    restart and the failed append each repost plain (`never an unmarked
    repost: "posted by a journal that is gone"`); the lost journal's dead
    letter is posted again (`never posted again stopped holding`);
  - MAJOR 3: gateway keys `["K:0","K:0"]` and `["K:0","K:1","K:1"]`: the
    text, then the template, never reached Meta;
  - MINOR: `the join held at the gateway while its keeper bind is owed
    stopped holding` (acked while the bind failed).
- `make_red.py` (`python3 -I assets/pr-evidence/gp-wf4e/make_red.py <out>`,
  run from a clean checkout of the round head; rebuilt at eb8afa07) makes
  two scratch trees:
  - `ba915531/`: the gate's head with the round's gate.test.js:
    `node-gate-RED-ba915531-rebuilt.txt`, 13 of 13 fail (the first run's
    10 plus the crash test, the self-review's NOTE 5 key reuse and the
    Fable read's order);
  - `v1/`: the round head with the restore gap written AFTER the head read
    (the round's first cut of the MAJOR 1 fix): `node-gate-RED-v1-crash.txt`,
    12 pass and the crash test fails (`never an unmarked duplicate`): the
    detection held in memory across the head read is lost to a crash once
    acceptances advanced the watermark; hence the gap line at the detection.
- `node-note5-key-reuse-RED.txt`: the self-review's NOTE 5 before the keys
  carried the content: the reused caller key went to the gateway as the
  same key twice and "another message" never reached Meta.
- `pytest-wording-binds-RED.txt`: the status and doctor wording (watermark
  NOT IN the store; the owed bind shown) before the change: 3 fail.
- `pytest-head-unknown-RED.txt`: the two new pytest cases before the
  surfaces changed: the doctor's restore row printed `(sequence null)`,
  clear-gap `sequence null`.
- `node-fable-minor1-RED.txt`: the order test before the token: `an order
  opens the window in the ledger, as at the gateway` fails.

## GREEN, at eb8afa07 (the round's last code commit)

- `node-GREEN.txt`: `cd whatsapp/adapter && pnpm exec node --test
  --test-concurrency=2 'test/*.test.js'` (the package's `npm test`): 173
  tests (160 at ba915531 plus gate.test.js's 13), 173 pass, 0 fail, 86.5 s,
  load 9.5-12.
- Four full runs at eb8afa07, ALL ON RECORD: three had ONE wait timeout
  each, a different test each time, never a wrong outcome, and each passes
  alone (`node-run1-...-11d-first-post-timeout.txt`: (11d) waiting for its
  first POST, alone 10 of 10; `node-run2-...-pipeline-partial-timeout.txt`
  and `node-run3-...-read-receipt-timeout.txt`: pipeline.test.js, a file and
  a fake this round does not touch, a fixed pump then an un-pumped ack
  wait); the fourth is `node-GREEN.txt`. The same host, the same flags, at
  load 11-14: ba915531's untouched suite 2 of 3 runs clean (the third timed
  out "a failed receipt reposts three times", the same pipeline class); the
  head's suite without gate.test.js 2 of 2 clean. gate.test.js itself is
  light: 0.70 s user CPU in 8.7 s (pipeline.test.js 2.06 s in 6.2 s). The
  class is the gp-9hf1 follow-up (`flake-diagnosis.md`).
- `pytest-GREEN.txt`: `env -u GC_TEMPLATE uv run --with pytest --with pyyaml
  --with jsonschema python3 -m pytest whatsapp/tests -q`: 44 passed (42 at
  ba915531 plus the two unknown-head cases; the status case now pins the
  owed-bind note and the new gap wording).
- `make-test-gascity-tail.txt`: `make test-gascity`: 831 passed, 113
  skipped (the same counts as gp-9hf1's run at ba915531; the round changes
  nothing outside whatsapp/ and this directory).

## The load flake

`flake-diagnosis.md`: review.test.js Minor 6 and restore.test.js (11a),
(11g) under load; Minor 6 fails 1 in 8 at ba915531 itself; the instrumented
runs show nothing of the round's code ran; the conversion is its own commit.

## Self-review

`self-review-r1.md`: a fresh read-only subagent at 4de5efdf, briefed with
the gate record and the delta only: CLEAN, 0 CRITICAL / 0 MAJOR / 1 MINOR /
5 NOTE; it re-ran the RED and GREEN on its own copies. Folded in eb8afa07:
the MINOR (an owed bind shown by status), NOTE 2 (the bind retry inside
poll's try), NOTE 4 (the gap wording), NOTE 5 (a gateway key names one
message: the caller key hashed WITH the content), NOTE 3 as a comment and a
design question; NOTE 6 (migration) not folded (never deployed).

## Commits

- `b7474ae2` fix: the gate's three MAJORs and the MINOR, with gate.test.js.
- `4de5efdf` test: the load flake's gap and read-back waits pump until the
  outcome lands (pre-existing at ba915531).
- `1878a086` fix: an order moves the window ledger (the Fable read r1's MINOR 1).
- `eb8afa07` fix: the self-review r1 folds.
- this evidence commit.

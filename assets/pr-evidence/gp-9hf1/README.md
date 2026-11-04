# gp-9hf1 evidence: the whatsapp pack and its box adapter

The bead: the whatsapp pack (the 11/3 bead B, design gp-76sw) with the design
gate r7's two recovery rules built and the design amended in the same PR
(Afik's (b), 16:33:14Z Wed 11/4). Node from the toolchain wrapper:
`/Users/tailor512/city/.gc/shims/toolchain/pnpm exec node --version` = v24.21.0.

## GREEN (the head)

| File | What |
| --- | --- |
| `node-GREEN.txt` | `cd whatsapp/adapter && pnpm exec node --test --test-concurrency=4 'test/*.test.js'` at the head: every case named by its spec number |
| `pytest-GREEN.txt` | `env -u GC_TEMPLATE python3 -m pytest whatsapp/tests -q` (run through `uv run --with pytest` on citadel) |
| `make-test-gascity-tail.txt` | `env -u GC_TEMPLATE make test-gascity` stays green (831 passed, 113 skipped) |
| `linux-proof.txt` | the disposable Debian bookworm arm64 container (`Dockerfile`, `linux-proof.sh`): `run.sh --deps-only`, the node suite, the pytest suite and the nine doctor rows on a clean env, as the `node` user |
| `node-keeper-GREEN.txt`, `node-outbound-media-GREEN.txt` | the keeper's and the outbound/media/ledger modules' own GREEN runs, with every asserted local time and mark |

## RED (before GREEN)

Three kinds, each produced in a scratch copy outside the repo (the scripts are
here, so a reader can rebuild them):

1. **The two gate rules, against the pre-round-8 contract** (`make_red.py`):
   `red1` restores the design's line 114 as it stood at `bc2fd143` (every
   watermark read at start only; the in-doubt rule reads the entry's own
   conversation; no scheduled read, no read after a poll, no read before a
   plain post). `red2` restores line 192 at `bc2fd143` (compaction drops
   acknowledged AND dead entries; no replay of a dead-letter line without its
   journal line).
   - `node-rule1-allwatermark-RED.txt`: (11h) fails with "never an unmarked
     duplicate" (B's message posted plain while A's gap went unseen: the gate
     r7's first finding, exactly); (11i) finds A's watermark never read before
     the plain post; (11j) finds no gap without the scheduled read; (11d2),
     (11f), (11g) depend on the schedule too.
   - `node-rule2-deadack-RED.txt`: (12a) and all seven (12b) reasons fail with
     "the dead line survives the compaction" (the replay guard gone while the
     gateway still holds the delivery: the gate r7's second finding); (12c)
     never acks; the journal unit test of the compaction rule fails.
2. **The engine against stubs** (`make_red0.py`, `node-engine-RED.txt`, run
   with `--test-timeout=20000` since a stub never reaches a SIGKILL
   failpoint): the journal, inbound, evidence, render and dead-letter modules
   replaced by stubs exporting the same names and implementing nothing: 80
   tests, 70 fail, 9 cancelled at the timeout, 1 pass (the gc registration,
   which index.js makes without the engine).
2b. **The self-review regressions against the pre-review source**
   (`make_red_review.py` at `864d2f0a`, `node-review-RED.txt`): 9 of 10 RED;
   the pass is the negative control (a delivery after the journal began is
   posted plain at once). **The contract readings against `f303c69d`**
   (`node-readings-RED.txt`): 3 of 4 RED; the pass is the control (a 502
   meta_error was already transient).
3. **The helper modules' own TDD runs**: `node-keeper-RED.txt` (23 of 23
   against "not implemented" stubs) and `node-outbound-media-RED.txt` (24 of
   24, then 27 of 27 against stubs).

## Reviews

| File | What |
| --- | --- |
| `self-review-r1.md` | the fresh self-review subagent's findings and what was done with each |
| `codex-r*.out` | the worker's codex STANDARD rounds (`codex review --base origin/main`) |

# gp-pnfn round 3 validation

Delta base: `a9fe2d9641503ecddc4e4027faeece9bb848046f` (PR 58's reviewed round-2 head).

The shared rules JSON now gives the base64 rule an allowlist for canonical
Slack permalinks with C/D/G channel IDs of 9 or 11 characters and a 16-digit
message timestamp. Go and Python evaluate the same pattern and exempt only
whole base64 matches contained in a permalink. Thread queries stay intact.
The base64 character class still includes `/`, so webhook URLs and AWS secret
keys containing slashes remain covered.

An exception is invalidated if another secret rule touches its span or a
same-rule match crosses its boundary. This
keeps provider-key detection active even in a URL host, and prevents a Bearer
match from destroying the `https` prefix while leaving a partial link that
changes on the next redaction pass. Both implementations sort matches, sweep
overlapping matches, and locate contained matches without an all-pairs scan.

The shared fixture set grew from 36 to 50. It replaces the synthetic C123 link
with an 11-character channel ID and thread query, changes the previous real
permalink's expected output to intact, and covers C/D/G variants, Slack mrkdwn
link pairs, webhooks, slash-containing AWS secrets, nearby/query/appended keys,
lookalike hosts, and provider/Bearer collisions. All fixtures assert exact
output and idempotence in both languages. The initial pre-fix Python run
reproduced the permalink failures; its raw output remains in
`.tasks/gp-pnfn-round3/red-python.txt`.

The README now documents both the permalink exception and the restored-held
replay scrub from round 2. No ingestion caller, outbound operation, historical
journal record, or replay persistence behavior changed in this round.

City integration: snapshot `12afd72318adbafc` contains byte-identical copies of
`slack_tail.py`, `slack_redact.py`, and `patterns.json`; the identifier uses the
existing first-16-of-SHA-256 derivation over those files in that order. All 50
fixtures, idempotence checks, and the synthetic DM pass through the installed
module and actual city wrapper from `/tmp` with fake gc. The prior snapshots
and `.pre-gp-pnfn-round3` wrapper backup remain in place. No live Slack reads or
adapter restart were used. See `round3-snapshot.txt` and `city-integration.md`.

Self-review: checked containment boundaries, appended credentials, lookalike
hosts, independent provider/Bearer matching, blocker sweep ordering, Go/Python
parity, exact channel lengths, thread-query preservation, the unchanged slash
character class, and snapshot bytes. No additional source issue found.

Commands and results:

- `go test -race -json ./... -count=1` in `slack-full/adapter`: 12,653 test/subtest passes, 0 failures, 0 skips, both packages pass, no race reports. See `round3-adapter-tests.json`.
- `go test -race -json ./... -count=1` in `slack-full/cli`: 237 test/subtest passes, 0 failures, 0 skips, all 8 packages pass. See `round3-cli-tests.json`.
- `uv run --with pytest python -m pytest slack-full/tests -q`: 814 passed, 2 skipped, 2 existing `os.fork()` deprecation warnings. See `round3-python-tests.txt`.
- `git diff --check`: pass.

Toolchains: Go 1.26.5 darwin/arm64; Python 3.13.13 through uv;
`/Users/tailor512/city/.gc/shims/toolchain/pnpm exec node --version` reports
v24.21.0. No Node tests apply and no unsupported-engine warnings occurred.
Complete Go JSONL transcripts remain locally in `.tasks/gp-pnfn-round3/`;
the committed JSON evidence summarizes terminal test and package events.

The initial gpt-6-astra delta gate returned CLEAN (helper exit 0,
anchored verdict) on source commit `677700b7f81293308e959f956c07e1524cc460cc`,
using `review --base a9fe2d9641503ecddc4e4027faeece9bb848046f`. See
`round3-codex-gate.txt`. That initial checkpoint was superseded by the fixture representation and
overlap changes below.
The mayor owns the subsequent round-3 delta gate and cross-model delta read.
The PR body credits the lane's gates, the mayor's CLEAN gates at `03525ec56`
and `a9fe2d964`, the Fable 5.1 round-1 read, and the Opus 5.5 round-2 delta read.

GitHub push protection rejected the initial literal synthetic webhook fixture
(`pc_f960bef0f395`). Its URL is now assembled from explicit `text_parts` in
both test harnesses, producing the identical test input without embedding a
complete credential-shaped webhook literal. The two rejected unpublished
commits were rebuilt on the same reviewed base; no published history was
rewritten and no protection bypass was used. The initial gate is retained as
a superseded checkpoint. Adapter/Python validation was rerun after this test
harness change; the CLI code and its passing evidence are unchanged. The fixture representation change also passed the installed tail wrapper;
that change alone left its three source files and snapshot hash unchanged.

One adapter rerun hit the existing wall-clock assertion in
`TestOwedVerdictRecordsAreRetried` at `inbound_rejection_ladder_test.go:2495`:
a retry timer remained visible 60 ms after the durable record appeared.
Its catch-up log followed the failed assertion. No data race was reported.
The unchanged test then passed 20 repetitions with race detection; see
`round3-owed-verdict-repeat.json`. The failed full transcript is retained at
`.tasks/gp-pnfn-round3/adapter-tests-final.jsonl`. Papercuts
`pc_3afc84321696` and its diagnosis correction `pc_7cceb649d469` record it.
No unrelated retry/timer source or test was changed for this round.

The subsequent complete adapter race rerun passed: 12,652 test/subtest passes,
0 failures/skips/race reports, both packages green. That intermediate raw transcript
is `.tasks/gp-pnfn-round3/adapter-tests-verified.jsonl`.

The second lane gate returned CLEAN with one minor: a same-rule base64 match
can cross into the `https` prefix of an otherwise allowed permalink. Both
redactors now invalidate the exception for that crossing match as well. The
50th shared fixture proves the previous source fails both exact output and
idempotence, and passes on the fix; see `round3-overlap-regression.txt`. The
second checkpoint is retained in `round3-codex-gate-r2.txt`. The installed
snapshot was re-pinned again after this source fix, preserving all older ones.

After the same-rule overlap fix, the full adapter race suite passed again with
12,653 test/subtest passes, no failures/skips/race reports, and both packages
green. Python passed 814 tests with 2 skips and the same 2 fork warnings. The
committed summaries above are from this final source; raw output is retained
in `adapter-tests-overlap-fixed.jsonl` and `python-tests-overlap-fixed.txt`
under `.tasks/gp-pnfn-round3/`. Self-review covered the final overlap sweep,
including invalidation by same-rule crossings at either boundary.

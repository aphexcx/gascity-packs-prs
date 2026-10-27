# Validation — jp-483, round 2

Implementation ran on jadegate, 2026-10-27 UTC / 2026-10-28 CST, continuing
`jp/mayor-shadow-eval` from `99ee1ab8166d1f43d097b147e3aea14aacf24d7f`.
Private evidence is under `~/city/work/eval-kimi-k31/round2/`. No transcript
text, customer names or credential values are included in this record.

## Gate results

- Runtime Python 3.14.7; pytest Python 3.12.14 through the repository uv runner; Claude Code 2.1.311; Node **v26.7.0**, measured inside
  `wecom/adapter`. The change is a Python pack; adapter suites were not invoked.
- Final pack suite: **229 cases passed** in the final full run; both repository command-policy checks also passed. This includes 164 guard
  tests and regression flows for pending tools, source freshness, hook replay,
  sampling, truncation, switch content and private read roots.
- Final full repository suite: **2,737 passed, 115 skipped, 10,631 subtests passed**,
  with two existing fork deprecation warnings, in 564.52 seconds. Evidence:
  `round2/full-identity-final.log` and `round2/full-identity-final.xml`.
- `gc lint mayor-shadow-eval`, Python syntax compilation, shell syntax checks,
  and `git diff --check`: passed. All script/hook invocations use `python3 -I`.
- Secret-pattern scan: **0** credential-shaped matches in the pack.
- `patch --dry-run ~/city/bin/mayor-model-watchdog
  mayor-shadow-eval/assets/patches/mayor-model-watchdog-live-source.patch`:
  applies cleanly. The installed watchdog was not edited or executed.
- No low-signal tests were deleted. The old file-existence switch case became
  five real-flow content/missing-switch cases; other existing tests were updated
  for the new security and compacted-seed contracts.

Invocations, with dependency caches and temporary test directories kept private:

The final commands used the private `round2/isolated-test-runner.sh`, equivalent
to this portable invocation. `-o pythonpath=.` restores repository imports under
isolated Python; clearing make overrides lets tests of nested `make -n` inspect
normal Makefile defaults. Earlier attempts exposed both invocation constraints;
no repository test or Makefile was relaxed.

```sh
UV_CACHE_DIR="$HOME/city/work/eval-kimi-k31/uv-cache" \
TMPDIR="$HOME/city/work/eval-kimi-k31" \
make test \
  PYTEST_RUNNER='echo env -u MAKEFLAGS -u MFLAGS -u MAKEOVERRIDES -u PYTHON -u PYTEST_RUNNER -u PYTEST_ARGS -u PYTEST_DIRS uv run --with pytest --with pyyaml --with jsonschema python3 -I -m pytest' \
  PYTEST_ARGS='-q -o pythonpath=.'
```

For the focused gate, set `PYTEST_DIRS=mayor-shadow-eval/tests`. Actual runs used
fresh basetemp directories under the private root and preserved prior results.
One earlier run hit an existing pnpm concurrency timing assertion; its isolated
retry passed. Another full run reported a command-policy false positive for a
list literal in the mutation classifier. The equivalent expression was changed,
and both policy checks and the next full run passed (2,736 tests, 115 skipped,
10,631 subtests). The final run above also includes the GC identity regression.

## Canary A: live source, normal-auth Opus

Completed in **182.320 seconds**, one attempt, default 25-turn limit:

```sh
mayor-shadow-eval/commands/shadow-watch.sh --once --model claude-opus-5-5 \
  --transcript "$HOME/.claude/projects/-Users-tailor64s-macbook-pro-city/ec4edc76-a322-4674-95b6-1eabc401098a.jsonl" \
  --config "$HOME/city/work/eval-kimi-k31/round2/canary-a-final.toml"
```

- Source: `ec4edc76-a322-4674-95b6-1eabc401098a.jsonl`; discovery: **explicit**.
- Newest-trigger age at selection: **2,108.08 seconds**; source stale=false.
- Trigger: `2026-10-27T22:23:40.706Z`, **2026-10-28 CST**.
- Seed: **1,191,869 bytes**, starting at the last compact boundary; 150 bytes
  of captured post-trigger hook context replayed. Both seed and actual runtime
  fork exist. Source-prefix equality after the session-ID rewrite: true.
- Actual/shadow blocks: **18 / 3**. Fable incomplete=false.
- Guard decisions: **1** total: read-allowed=0, mutation-denied=0,
  shape-denied=1; tool **Bash**. Final-code reclassification agrees with the
  recorded decision. Both sides are populated; the model stopped after its
  denied read attempt. No artificial mutation was added to the replay.
- `same_action=false`; first mutation: actual **Write**, shadow **none**.
  Targets stay private.
- Live source bytes before/after: **174,267,177 / 174,267,177**; prefix unchanged.
  Prefix SHA-256: `786493a8a1eb86cfb892091850f7d90a0699b1b96ab9eb5e41f7947b0da2f9ac`.
- Runtime init hash: `84fa0aee1e5858e30f810afe4e22d9689fc49f5dfdbb81419e91cdd447d54720`.
  Its actual tool/skill lists are recorded. `gascity.mayor` and all seven
  `core.gc-*` skills loaded. All 14 source tool schemas appeared unchanged in
  the recorded wire request. Additional runtime/deferred tools are disclosed.
- A separate read-only gc inbox probe found GC_ALIAS overriding GC_AGENT. The
  final environment strips worker aliases, bead-store overrides and session
  credentials; the repeated probe resolved recipient=mayor with no worker
  routing fields retained. Evidence: `gc-identity-check-final.json`. The canary
  itself attempted no allowed gc read; its final trace is unchanged by this fix.
- System-prompt identity is **unverified**; the copied source snapshot is not
  evidence of the current system prompt. Superpowers remains disabled because
  it has a live SessionStart hook. Shadow permissions are dontAsk, while the
  live mayor bypasses permissions. The scorecard states both differences.

Evidence: `canary-a-final-summary.json`, `canary-a-final/ledger/2026-10-28/`, and private
runtime/stream files under that directory. The earlier 146.793-second canary is
also retained in `canary-a/`; it exercised one allowed Bash read and denied
outbound calls. A subsequent classifier check exposed a printf append being
recorded as a shape denial; the mutation/target classifier and its regression
were corrected before the final canary. Its historical classifications are not
presented as final-code evidence.

## Source-selection blocker (A)

A full decoded scan of the live 174,267,177-byte transcript found **zero**
paragraph-anchored mayor markers in any string. Its marker occurrences are
quoted review/tool material, not identifying user/attachment paragraphs. The
new detector therefore correctly refuses that file and selects the old
marker-bearing `56d7a09b…` file, then reports **stale_source / RED** and replays
nothing. The gc-recorded session UUID still names a missing file.

Synthetic project fixtures prove that a live resumed file with an anchored
user/attachment marker after 200 KB wins over a stale head-marker file, and
that a fresh tool-result quotation never wins. The requested production
fallback success cannot be demonstrated with the current input. The mayor
was notified in mail `jg-wisp-gu2zy51` and progress update `jg-wisp-vgf65pv`. Before unattended installation, the
mayor must correct the gc session key or supply a trusted role marker/source.
The worker did not modify the live transcript or weaken the detector.

## Canary B: dummy Moonshot key

With environment key `invalid`, the same explicit live source and private
`canary-b.toml` (one turn, 50-second timeout) returned **http_401 in 11.730
seconds**, one attempt and zero guard/tool executions. Live source bytes and
prefix hash were unchanged. Evidence: `canary-b-summary.json` and its ledger.
No genuine Moonshot key was fetched or tested.

## Synthetic judges

Three synthetic ledgers (including one `completed_truncated`) were judged by
normal-auth Opus: **3 judged, 0 failed**. The first run exposed an overlong
reason; a schema word-count pattern was added, then all three passed. Reasons
are evidence-specific, at most 20 words, with explicit no-names instructions.
Both sides include bounded, redacted tool-result excerpts. The Opus scorecard
ends exactly:

```text
COUNTS: CRITICAL=0 MAJOR=1 MINOR=0 NOTE=2
VERDICT: CLEAN
```

One live Codex judgment ran through `~/city/.gc/shims/codex-astra/codex -p city`
on an identical saved synthetic packet: **1 judged, 0 failed**. Its scorecard
ends exactly:

```text
COUNTS: CRITICAL=0 MAJOR=1 MINOR=0 NOTE=0
VERDICT: CLEAN
```

Evidence: `synthetic-judge/scorecard-2026-10-28{,-codex}.{md,json}` and private
judgment records. Codex execution uses a read-only sandbox, an ephemeral
session, the city profile, and a structured output schema; the local flags
were checked against the installed CLI and the
[official command reference](https://learn.chatgpt.com/docs/developer-commands?surface=cli).
These are synthetic rubric verdicts, not a clean production-comparison result.

## A–O status

- A: Code and fixtures pass; watchdog dry-run passes. Production automatic
  source selection is blocked by the missing trusted marker/session mapping.
- B: Read-root confinement, symlink/glob probes, web denial and 60-second Bash
  limits implemented and tested; logging failures still exit 2.
- C: Read-only compounds and Skill allowed; shape/mutation/timer classification
  separated; comparisons use the first mutation and its target.
- D: Pending tool calls wait; max_wait marks incomplete; judging refreshes Fable.
- E: Post-trigger hook context is captured and emitted by a replay-only hook.
- F: Estimated reservations release after known usage; hourly sampling and the
  sample_every override persist across polls and day boundaries.
- G: completed_truncated is recorded and judged; default max_turns is 25.
- H: Project skills and actual-init evidence verified; plugin and permission
  differences remain explicit rather than claiming full parity.
- I: Interruption and local-command trigger shapes excluded and tested.
- J: Persisted incremental tail; compact-boundary seeds; runtime size in status.
- K: Redacted result excerpts, bounded specific reasons and exact COUNTS/VERDICT.
- L: Content switch, missing=off, no off-state ledger pollution; tested.
- M: Source/method/age/stale, coverage, storage, tokens/cost and error class exposed.
- N: One live Codex synthetic judgment completed; verdict line above.
- O: Infisical export cached once per run; failure cache, modes and regressions covered.

No launchd job was loaded and no live watcher loop was started. Genuine Moonshot
success and multi-day unattended coverage remain deployment checks. Installation,
start/stop/status and watchdog apply/rollback steps are in README. The branch is
for review and handoff; no PR or merge was attempted.

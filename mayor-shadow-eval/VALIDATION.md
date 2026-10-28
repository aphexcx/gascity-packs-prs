# Validation — jp-8e8, round 5

Continued `jp/mayor-shadow-eval` from `7b1a145a` on jadegate, Darwin 27.0.0.
BSD readers stop parsing at the first operand. The alternate getopt parse also
checks directory operands, including a repeated directory-looking search
pattern. Unknown readers remain denied; shallow `ls ~/city/work` still works.

The shadow now runs under a generated Seatbelt read/write policy. Current-turn
runtime storage is separate from previous forks and ledgers. The hook reads a
private guard config snapshot; its decision sidecar is writable but unreadable.
The normal judge write sandbox is unchanged. `sandbox` defaults on when the
binary exists; explicit off is visible in watch/status and every turn ledger.

- Final repository runner: **467 passed in 33.01 seconds**: **463 pack tests**
  (including **51 new round-5 cases**) and **4 repository command-policy tests**.
  Real BSD utilities demonstrate the two bypasses against synthetic fixtures;
  the real hook denies them. OS tests bypass the hook entirely and assert EPERM
  for cat and du, future service secrets, ledgers, previous runtimes, live config,
  credentials, extra exclusions and symlink targets. They also exercise allowed
  context reads, write confinement, sandbox-off status and the real replay/hook
  subprocess chain. No tests were deleted. The captured-context test now follows
  the ledger's runtime context path.
- The real hook, separately invoked with `cwd=~/city`, denied both
  `du work/dispatch/mayor-shadow-eval-1028-round5.md --exclude .` and
  `ls -R work/dispatch/mayor-shadow-eval-1028-round5.md --ignore .` with the
  read-shape reason. Neither utility was executed against the live city.
- Final live `--model claude-opus-5-5` canary: **completed**, normal auth,
  `sandbox: on`, exit 0, no errors, **57.991 seconds**, seed **2,627,251 bytes**.
  Source `ec4edc76-a322-4674-95b6-1eabc401098a.jsonl`, discovery `explicit`,
  trigger `2026-10-28T02:18:22.787Z`; newest-trigger age at start **2.45 seconds**.
  Guard decisions: Bash read-allowed 1, Bash shape-denied 3, WebSearch
  mutation-denied 1, Write mutation-denied 2. `same_action=false`.
  Runtime fork exists; seed prefix and source tool schemas retained. Live source
  grew from **177,223,775** to **177,289,430 bytes**, existing prefix unchanged.
  Actual runtime instruction attachments include `MEMORY.md`; the final prompt
  references it. The generated profile matches the final implementation.
- Neither `~/city/CLAUDE.md` nor `~/.claude/CLAUDE.md` exists on this host. A
  separate normal-auth Opus startup probe supplied a synthetic CLAUDE.md in its
  private runtime: **exit 0 in 6.566 seconds**, expected instruction response,
  `sandbox: on`. OS tests also verify CLAUDE.md and memory reads. No live context
  file was created or edited. No allow-list widening or parser-only fallback was
  needed. The existing `runtime_system_prompt_verified=false` fidelity limitation
  remains explicit.
- `gc lint mayor-shadow-eval`, Python AST/shell syntax, `python3 -I` entrypoints,
  `git diff --check`: passed. Secret-pattern scan: **0**; no values printed.
- Test Python **3.12.14** via uv; host Python **3.14.7**. Node **v26.7.0**, measured
  inside `slack-full/adapter`; this Python pack has no Node test suite.
- Implementation self-review covered both option regimes, duplicate operands,
  Seatbelt regex quoting, future exclusions, symlinks, child-process inheritance,
  runtime exceptions, bundled/default config startup and fail-closed launch.
  No fresh external review gate is claimed.

Exact final test invocation:

```sh
UV_CACHE_DIR="$HOME/city/work/eval-kimi-k31/uv-cache" \
TMPDIR="$HOME/city/work/eval-kimi-k31" \
make test PYTHON='python3 -I' \
  PYTEST_DIRS='mayor-shadow-eval/tests tests/test_no_bare_bd_commands.py tests/test_no_gc_sling_wrapper.py' \
  PYTEST_ARGS="-q -o pythonpath=. -o cache_dir=$HOME/city/work/eval-kimi-k31/round5-cache --basetemp=$HOME/city/work/eval-kimi-k31/round5-reviewed"
```

Content-free evidence under `~/city/work/eval-kimi-k31/`: `round5-reviewed.log`,
`round5-real-hooks.json`, `round5-canary-final/summary.json`,
`round5-context-canary/summary.json`. Private replay artifacts remain there;
no transcript text or credentials are committed. Installation/start/stop steps
remain in README; no service was installed or loop started. Earlier full-repo,
dummy-key and judge gates stand as recorded below.

# Validation — jp-n1e, round 4

Continued `jp/mayor-shadow-eval` from `f7b22c3a` on jadegate. The change replaces
independent recursion/operand scans with one bounded option parser. Required
arguments consume `--` as data; glued short values and long equals forms follow
the same grammar. Unknown/missing options and pattern-file reads fail closed.
Find scopes and literal directory operands after a real terminator are checked;
ordinary shallow `ls ~/city/work` remains allowed.

- Pack suite: **412 passed** in 27.23 seconds, including **100 new real-hook
  regressions**. The initial 88-case regression run exposed **29 failures** in
  the old parser. Four new cases also execute grep against synthetic excluded
  fixtures, confirm traversal, and verify that the hook denies those commands.
  No tests were deleted.
- Real hook invoked separately with `cwd=~/city` and
  `grep -e -- -r secret .`: **deny**, with the round-2 read-shape reason. The
  command itself was never executed against the live city.
- Repository command-policy checks: **4 passed** in 3.10 seconds.
- `gc lint mayor-shadow-eval`, Python/shell syntax, entrypoint/hook isolation,
  and `git diff --check`: **passed**. Every Python execution used `-I`.
- Secret-pattern scan: **0** credential-shaped matches; values were not printed.
- Test Python: **3.12.14** through the repository uv runner; host Python:
  **3.14.7**. Node: **v26.7.0**, measured inside `slack-full/adapter`; this
  Python-pack change did not run a Node suite.
- Implementation self-review covered option arity, bundles, optional color
  arguments, default scopes, literal directory operands, and BSD/GNU ambiguity.
  Ambiguous `ls -w/-T/-I` and `stat -f` forms are denied. Prior round-3 judge and
  source-selection behavior remains covered by the pack suite.

Exact pack invocation:

```sh
UV_CACHE_DIR="$HOME/city/work/eval-kimi-k31/uv-cache" \
TMPDIR="$HOME/city/work/eval-kimi-k31" \
make test PYTHON='python3 -I' PYTEST_DIRS=mayor-shadow-eval/tests \
  PYTEST_ARGS="-q -o pythonpath=. -o cache_dir=$HOME/city/work/eval-kimi-k31/round4-pytest-cache --basetemp=$HOME/city/work/eval-kimi-k31/round4-reviewed"
```

The policy invocation uses the same runner with
`PYTEST_DIRS='tests/test_no_bare_bd_commands.py tests/test_no_gc_sling_wrapper.py'`
and basetemp `round4-policy`. Evidence is retained under
`~/city/work/eval-kimi-k31/`: `round4-red.log`, `round4-reviewed.log`,
`round4-policy.log`, `round4-gates.json`, and `round4-hook-decisions.jsonl`.
Earlier rounds' provider/full-repository gates stand as recorded below; this
round did not install the pack or start a live replay.

# Validation — jp-ake, round 3

Continues `jp/mayor-shadow-eval` from
`01168f68304fde85f449e40aa6b17846f2ffbbf0` on jadegate, 2026-10-28 UTC/CST.

- Pack suite: **312 passed** in 24.35 seconds, including **83 new cases**:
  64 directory-scope/hook cases and 19 judge CLI cases. No tests were deleted.
- Both reported commands, `grep -Rn secret .` and `grep -d recurse secret .`,
  now deny with the shape reason in a synthetic city through the real hook.
  Directory operands, implicit recursive reads, combined flags, option values,
  and the specified safe reads are covered. Shallow `ls ~/city/work` is the
  explicit listing exception; recursive listings check excluded descendants.
  Unsupported `tree` remains denied by the command grammar.
- Real judge subprocess fixtures cover parse errors, schema rejection, model
  errors, process failures and timeouts, alone and alongside successful turns.
  Each produces BLOCK, failure counts/turn lines, and exit 1. Multiple failures
  retain the ledger's sequence numbers. All-good fixtures remain CLEAN/exit 0;
  empty or entirely unjudged days produce BLOCK with `no judgments`/exit 1.
- All Python invocations use `python3 -I` or the equivalent interpreter `-I`.
  Tests used Python **3.12.14** via the repository uv runner; host Python is
  **3.14.7**. Node **v26.7.0** was measured inside `slack-full/adapter`.
- Secret-pattern scan: **0** credential-shaped matches across the pack;
  credential values are never printed. `git diff --check`: passed.
- Source discovery, explicit live-source configuration, and `stale_source`
  checks are unchanged. These are the round-3 pack gates; the round-2 full-suite
  and live-provider evidence remains below. No installation was performed.

Exact pack invocation (private paths retain synthetic fixtures and output):

```sh
UV_CACHE_DIR="$HOME/city/work/eval-kimi-k31/uv-cache" \
TMPDIR="$HOME/city/work/eval-kimi-k31" \
make test PYTHON='python3 -I' PYTEST_DIRS=mayor-shadow-eval/tests \
  PYTEST_ARGS="-q -o pythonpath=. -o cache_dir=$HOME/city/work/eval-kimi-k31/round3-pytest-cache --basetemp=$HOME/city/work/eval-kimi-k31/round3-pack-tests-delivery"
```

Output: `~/city/work/eval-kimi-k31/round3-pack-tests.log`.
Implementation self-review covered directory/implicit scopes, error reporting,
exit codes, persisted turn identifiers and unchanged source selection.

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

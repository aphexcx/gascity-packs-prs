# Validation — jp-rci

Implementation and canaries ran on jadegate, 2026-10-27 UTC / 2026-10-28 CST.
No live transcript text, customer identifiers or credential values are included
here. Private evidence lives under `~/city/work/eval-kimi-k31/`.

## Runtime and local checks

- Python 3.14.7; Claude Code 2.1.311.
- Node v26.7.0, measured with `node --version` from `wecom/adapter`. This is a
  Python-only pack; no Node adapter code changed or Node test suite was invoked.
- All command wrappers and the hook use `python3 -I`; Python sources compile and
  shell wrappers parse.
- `gc lint mayor-shadow-eval`: passed.
- Secret-pattern scan: no credential-shaped `sk-` or literal auth-token values
  in the pack. All fixtures are synthetic.
- Tests are included in the root Makefile and CI pytest invocation. No tests
  were deleted; the suite exercises a real child process and actual guard hook.

Test invocations used `UV_CACHE_DIR=~/city/work/eval-kimi-k31/uv-cache` and
`TMPDIR=~/city/work/eval-kimi-k31` to keep generated dependencies/artifacts in the
permitted work directory. The Makefile selected its documented uv fallback.

- `make test`: **2,645 passed, 115 skipped, 10,631 subtests passed** in 562.15 s.
  Two existing Python fork deprecation warnings came from the Slack outbound
  race test. Root CI/Makefile edits were unchanged throughout this run.
- Final `make test PYTEST_DIRS=mayor-shadow-eval/tests`: **138 passed** in 9.28 s,
  including **112 guard tests**. This rerun covers the final pack bookkeeping
  changes made while the wider repository suite was running.

## Canary A: normal-auth Opus

`commands/shadow-watch.sh --once --model claude-opus-5-5 --config
~/city/work/eval-kimi-k31/canary-a4.toml` completed in 78.043 seconds, one attempt,
with the shipped ten-turn default. The private config only changes the work
directory and explicitly retains the normal timeout.

- Both sides populated: 23 actual mayor blocks and 6 shadow blocks.
- Four hook decisions: Bash, Bash, Bash, Bash; all denied.
- `same_action = false`; first potentially mutating tool on each side: Bash.
  Targets and command text remain private in the ledger's full `diff` block.
- Source size before/after: 10,978,154 / 10,978,154 bytes. The full original
  prefix hash remained unchanged:
  `caac715b84384d91a3be374d273ee24b49a1046ec97390df10019b490ea42e90`.
- Seed prefix identical except session IDs; mayor role and instruction
  attachment retained; system prompt snapshot retained with SHA-256
  `a0515205c7e0704fd9d193a58bd71c23ac7b65d94d95eb5437f46e8affa1d71f`.
- All 14 source tool schemas and descriptions retained in the first recorded
  wire request. CronCreate, CronList and Monitor schemas also match the source's
  deferred-tool records. Claude adds its DeferredToolPlaceholder transport tool.
- Both seed and runtime-fork UUID files exist in the private runtime projects
  directory, and remain in place. The ledger records their absolute paths.

Evidence: `canary-a4/ledger/2026-10-28/` and `canary-a4-summary.json` under the
private root. Early checks caught and fixed partial-config hook loading and
false API-error classification from response text. A subsequent three-turn
canary reached its deliberately reduced limit; the ten-turn canary above passed.
Those earlier artifacts remain private for inspection.

## Canary B: invalid Moonshot credential

The same fork mechanics ran with the dummy environment credential `invalid`,
one model turn, and a 50-second timeout. It recorded `http_401` after **15.873
seconds**, one attempt, with zero tool executions. The source size and original
prefix hash remained unchanged. Evidence: `canary-b/ledger/2026-10-28/` and
`canary-b-summary.json` under the private root. No real Moonshot key was fetched
or exercised.

## Synthetic judge

`commands/shadow-judge.sh 2026-10-28 --synthetic --config
~/city/work/eval-kimi-k31/synthetic-judge.toml` used normal-auth Opus on three
synthetic ledgers and synthetic rule documents: **3 judged, 0 failed**. The
validated scorecard reports five criterion means, action-match percentage,
one violating turn, one human-intervention flag, examples, token/accounting
fields, median latency, and skipped/failed counts. Private artifacts:
`synthetic-judge/scorecard-2026-10-28.md` and `.json`.

The Codex spot-check path is covered through a synthetic shim subprocess and
packet-identity assertions. No live Codex judgment was launched; that option
belongs to the mayor.

## Deployment limitations

The supplied gc/watchdog detector selected a transcript ending October 12,
whose most recent completed trigger was **2026-10-12T07:52:52.377Z**. This proves
the fork mechanism on the latest available detected source; it does not prove
that the detector currently follows an October 27 live mayor. The mayor must
verify the source/session before installation.

Fork files intentionally use the private runtime projects directory, respecting
the bead's write boundary. Original transcript, settings and memory files are
read-only. Active plugins/MCP servers and inherited hooks do not launch; saved
context/tool schemas are retained. Guard restrictions and the static action
comparison's limitations are documented in the README.

No launchd job was loaded and no live watch loop was started. Genuine Moonshot
success, unattended operation over multiple days, and a live Codex spot-check
remain unverified. The real key and installation are the mayor's rollout steps.

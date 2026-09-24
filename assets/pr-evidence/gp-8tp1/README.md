# gp-8tp1: explicit reply threads and company routing

## Round 4: gp-1rqe

Continues draft PR #44 on gp-8tp1 from bcc16ff7208f2189a4e6ee5f3e5fc09c5acbf7dd.
The coalesced header now recommends only `--reply-to`, using the newest human
member's thread root or own timestamp. Older members use that same rule.
Count, channel, one-line header, and newest-human selection are preserved.
The bot-reaction anchor test also needed its expected flag updated.

An explicit matching company DM/MPIM with `--no-thread` keeps the company
route when that route already posts top-level. A different unbound conversation
or a threaded company pointer still refuses before any post. The module
docstring and help recommend `--reply-to`; `--turn-ts` remains a compatibility
form. Ordinary publishing now strips the same anchor whitespace as the guard.

Evidence added for this round:

- rendered-after-coalesced.txt: actual Go formatter output for channel and DM
  inputs, both top-level and threaded. Live DM events still bypass buffering;
  these DM cases verify the formatter's channel-independent anchor behavior.
- round4-tests-go-red.txt / round4-tests-go-green.txt: four top-level tests and
  four subtests fail at bcc16ff7 with only the regression tests changed, then
  all eight outcomes pass after the fix.
- round4-tests-python-red.txt / round4-tests-python-green.txt: the matching
  DM, matching MPIM, and whitespace cases fail; four refusal controls pass.
  After the fix all seven pass, with 557 deselected.
- round4-tests-go-full.txt: 1,186 top-level tests, 1,842 passing test/subtest
  outcomes, zero failures/skips; Go 1.26.5 darwin/arm64. Package output and
  counts decoded from `go test -count=1 -json ./...`, with GC_TEMPLATE unset
  and the prescribed ICU flags. The raw JSON is retained locally at
  /tmp/gp-1rqe-go-full.jsonl.
- round4-tests-python-full.txt: `env -u GC_TEMPLATE uv run --no-project --with
  pytest python -m pytest slack-full/tests -q`: 564 passed; two existing fork
  DeprecationWarnings. All prior explicit-target refusal tests remain green.
- round4-change.diff: zero-context diff from bcc16ff7 for this round's seven
  source, documentation, and test files. Earlier evidence below is historical.

Every transcript records its exact command and exit status. Tests intercept
outbound requests; no live Slack messages were sent. The prescribed
`/Users/tailor512/city/.gc/shims/toolchain/pnpm exec node --version` check reports
v24.21.0; these suites run on Go/Python. No unsupported-engine warning occurred.

The binding query's single-session-identity limit is recorded without change,
as requested. WeCom and company_hydration.go's `--turn-ref` contract are unchanged.
The shared origin remote names aphexcx/gascity-packs; fork2 names the actual
PR repository aphexcx/gascity-packs-prs, and is used for authenticated fetch/push.
Both origin/main and fetched fork2/main have the same merge base with this
branch: 7621e7b6a7f1db0a69c181d33b8c4e03f34fbc7a. Papercut: pc_0c5bbb08fafd.

Worker Codex STANDARD round 4 (gpt-6-astra, attempt 1) found no actionable
regressions and independently passed all 564 Python tests and affected Go
tests. Its sandbox reproduced the unchanged setgid-permission test failure;
worker full-suite results above passed outside the sandbox. See
round4-review-standard-1.txt and round4-tests-review-environment.txt.
Recurrence: pc_57ad64ab9acc. Review command:

```sh
codex review --base origin/main -c 'sandbox_workspace_write.network_access=true'
```

## Prior round: gp-8tp1

The prior round reuses candidate dc065d2f2a45b31942209109d13a568a889ea92c from
draft PR #43 by cherry-pick, then fixes its verified company-routing P1.
The gp-gu49 evidence remains historical; its BLOCKED verdict applies to the
text-only candidate before this guard fix.

## Behavior and scope

The registered Reply template uses `--reply-to {thread_ts}` for every inbound.
gc supplies the inbound thread root, falling back to its own message timestamp
for a top-level post. Verified in `renderExtmsgReplyInstructions` at core commit
92a7f383ff69a2b9a995239a6612fe0735c6f09d. The once-per-channel how-to explains
this in one sentence and retains `--no-thread` for an explicit top-level post.

A live company pointer cannot capture an explicit conversation or thread.
Explicit targets outside the company route use ordinary resolution when the
session has an active matching gc binding (all bindings are considered), or a
mention-only route. Otherwise the command refuses before posting, naming both
targets. A failed binding lookup also refuses. This applies to --reply-to,
--turn-ts, conversation-only overrides, and --via adapter. A thread-only
override resolves its conversation first. Matching company selectors retain
the acting-agent route, pinned against a newer wake before posting. DM/MPIM
company root turns post top-level, so an explicit thread on that root uses the
bound ordinary route or refuses as well. Implicit routing is unchanged.

The guard lives in slack-full/scripts/slack_chat_reply_current.py. The new
mixed-session regressions are in slack-full/tests/test_slack_chat_reply_current.py.
No other adapter message text or batching behavior changed. WeCom has its own
`gc wecom publish` how-to/template in inbound.js and index.js; it does not render
the Slack Reply line and is unchanged.

## Evidence

- rendered-before.txt and rendered-after.txt: source-derived full how-to and
  Reply line for both top-level and threaded synthetic inbounds; no live Slack
  messages were sent.
- tests-red.txt: old guard restored temporarily against the regression cases:
  23 failed, 2 passed, 530 deselected. Both explicit flags reproduce the defect.
- tests-red-dm.txt: the additional DM top-level case failed for both bound and
  unbound targets before the DM-family guard correction.
- tests-python.txt: final focused reply-template/reply-current suite.
- tests-python-full.txt: broader Python pack suite.
- tests-go.txt: full adapter suite, decoded JSON test output with counts.
- change.diff: zero-context diff against origin/main for the five changed
  production/documentation/test files, excluding evidence.

## Verification

Go 1.26.5 darwin/arm64: 1,185 top-level tests, 1,837 passing test/subtest
results, zero failures/skips. From slack-full/adapter:

```sh
env -u GC_TEMPLATE CGO_CPPFLAGS=-I/opt/homebrew/opt/icu4c@78/include CGO_LDFLAGS=-L/opt/homebrew/opt/icu4c@78/lib go test -count=1 -json ./...
```

Focused Python: 90 passed, 467 deselected. From the lane root:

```sh
env -u GC_TEMPLATE uv run --no-project --with pytest python -m pytest slack-full/tests -q -k 'reply_template_contract or slack_chat_reply_current'
```

Broader Python: 557 passed, two existing DeprecationWarnings from the fork-based
company outbound race test. No unsupported-engine warning occurred.

```sh
env -u GC_TEMPLATE uv run --no-project --with pytest python -m pytest slack-full/tests -q
```

These are Go and Python tests, not Node tests. The prescribed toolchain check
`/Users/tailor512/city/.gc/shims/toolchain/pnpm exec node --version` reports
v24.21.0; Node was not used to run these suites.

## Review and delivery

Worker Codex STANDARD round 1 (gpt-6-astra) finished with no actionable
defects in the latest diff and confirmed 90 focused Python tests pass. Command:

```sh
codex review --base origin/main -c 'sandbox_workspace_write.network_access=true'
```

The review sandbox reproduced the known unrelated setgid-permissions failure
(pc_bbccc243c44c); worker full Go tests passed, and the focused outside-sandbox
rerun passed one top-level test plus eight subtests. See
review-standard-1.txt and tests-review-environment.txt. Recurrence logged as
pc_ac8090bfb38a. No production change was made for that out-of-scope flake.

The mayor's gate, Fable read, founder merge decision, and installation follow
READY; this PR remains draft.

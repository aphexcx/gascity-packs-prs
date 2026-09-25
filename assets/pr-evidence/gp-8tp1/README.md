# gp-8tp1: explicit reply threads and company routing

## Round 7: gp-l3ie

Continues draft PR #44 on gp-8tp1 from ed04e6c4144b44d41efba183c2edca51fa1a1bb2.
The explicit-target guard now recognizes room-group participants even when
bind-room was invoked without --binding-owner. Individual bindings retain
priority; when none match, GET /extmsg/groups resolves the exact conversation
and GET /beads?label=extmsg:group:participant:v1:<group-id> reads active participant
records, following next_cursor. The session_id or stable session_name must
match the same session_identity_candidates set already used for bindings.
No extra /sessions lookup is introduced by the group check.

Discovery: slack_chat_bind_room.py POSTs /extmsg/groups and /extmsg/participants.
The local config mirror can outlive a participant removed through GC. The GC
source's humaHandleExtMsgGroupLookup returns the group without participants;
groupService.listParticipants and ResolveOutbound read live participant beads
with the group label, skip closed records, and decode session_id/session_name.
This patch uses those records through the existing public beads API; no GC or
adapter changes are needed. The ordinary outbound route retains its own final
authorization. Company selectors, DM/MPIM paths, and mention-only lookup are unchanged.

The regression invokes bind-room with intercepted API calls, then reply-current
with a live company pointer elsewhere. It covers ID, environment name, API alias,
API session name, binding owner, nonmember, closed membership, wrong group,
wrong workspace, absent/unavailable group, and membership on a second page.
The local config remains present in the stale-membership cases: it cannot
permit a post. All test requests are intercepted; no live Slack calls occur.

Evidence:

- round7-tests-python-red.txt: baseline plus tests only; 5 failed, 588 passed.
  ID/name/alias group members and the second-page member fail at the old guard;
  owner and refusal controls already pass.
- round7-tests-python-green.txt: 593 passed, two existing fork warnings.
  Both full runs: env -u GC_TEMPLATE uv run --no-project --with pytest python -m
  pytest slack-full/tests -q.
- round7-tests-go-full.txt: unchanged adapter suite; 1,186 top-level and 1,842
  total passing outcomes, no failures/skips. Exact command and Go version are
  in the transcript. No nonempty SLACK_*TOKEN environment values were present.
- round7-change.diff: zero-context source/test diff against ed04e6c4.

Toolchain check: /Users/tailor512/city/.gc/shims/toolchain/pnpm exec node --version
returned v24.21.0. These suites use Python/Go; no unsupported-engine warning.
The shared origin URL is aphexcx/gascity-packs; an authenticated explicit fetch
of aphexcx/gascity-packs-prs main confirms FETCH_HEAD, origin/main, and the PR
merge base all equal 7621e7b6a7f1db0a69c181d33b8c4e03f34fbc7a.

Read both Mayor comments: gate r4 blocks on this group regression; Fable r4
is CLEAN and assigns no extra fix. Record-only legacy limitation: a user-set
--kind thread (or dm on a C-channel) fails the guard's conversation-kind match
and may refuse a bound target; the Mayor holds that follow-up after this PR.

Worker Codex STANDARD round 7 (gpt-6-astra, high effort, first attempt) found no
actionable regressions. The known review-sandbox setgid failure passed in the
worker full suite and focused rerun (one top-level test/eight subtests): see
round7-review-standard-1.txt and round7-tests-review-environment.txt; recurrence
pc_0bfbf13952e5. The missing participant read endpoint is filed as pc_939e69f59749.
Mayor gate r5 and Fable read r5 follow READY; PR #44 stays draft.

## Round 6: gp-12bl

Continues draft PR #44 on gp-8tp1 from 3190c9af0f712192df46ae6b297f495fe302f8ae.
Implements option (a): before falling back to the ordinary route, refuse an
explicit target that conflicts with `--turn-ref`, `--origin-ts`, or a company
`--kind room|dm|mpim` override. The error names every supplied company selector
and the resolved target, and asks the caller to drop the selectors or target.
This happens before body loading or any POST. A fresh origin timestamp still
cannot authorize a different bound destination; matching company targets keep
the existing fresh/stale timestamp checks and acting-agent route. Without a
company selector, the round-4/5 bound-target fallback remains unchanged.

The Fable r3 comment on gp-12bl is CLEAN and assigns no additional fixes.
The round-6 source delta is limited to the fallback in reply-current; no
identity lookup, DM/MPIM routing, coalesced header, or Go source changes.

Evidence:

- `round6-tests-python-red.txt`: at 3190c9af with only the twelve new cases,
  eight conflicting-target cases fail because no refusal occurs; 573 pass,
  including four matching-target controls and all no-selector regressions.
- `round6-tests-python-green.txt`: all 581 pass. Both full runs use
  `env -u GC_TEMPLATE uv run --no-project --with pytest python -m pytest
  slack-full/tests -q`; both report the same two existing fork warnings.
- `round6-tests-go-full.txt`: unchanged full adapter suite, 1,186 top-level
  and 1,842 total passing outcomes, zero failures/skips; Go 1.26.5 darwin/arm64.
- `round6-change.diff`: zero-context guard/test diff from 3190c9af.

The tests intercept outbound requests; no live Slack calls were made, and the
Go runner verified that no SLACK_*TOKEN environment value was set. The required
`/Users/tailor512/city/.gc/shims/toolchain/pnpm exec node --version` reports
v24.21.0; these suites use Python/Go, with no unsupported-engine warning.
The shared origin remote names aphexcx/gascity-packs; explicit fetch/push URLs
name aphexcx/gascity-packs-prs. Local origin/main, fetched fork2/main, and the
PR merge base all resolve to 7621e7b6a7f1db0a69c181d33b8c4e03f34fbc7a.

Worker Codex STANDARD round 6 (gpt-6-astra, first attempt) returned one P2:
the existing guard does not recognize group participants without one-to-one
bindings. An offline comparison confirms the same refusal at 3190c9af and
with the round-6 fix. This bead excludes binding/identity lookup changes, so
the finding is recorded and escalated to mayor for separate work; this is
not a clean PR-level review. See `round6-review-standard-1.txt` and
`round6-review-group-route.txt`.

The review sandbox again stripped a directory's setgid bit in the unchanged
permission test. The worker full suite and focused rerun (one top-level plus
eight subtests) pass; see `round6-tests-review-environment.txt`.
Recurrence: pc_dbf30a3b63ae. The mayor gate r4 and Fable read r4 follow READY;
PR #44 stays draft pending review resolution and the founder merge decision.

## Round 5: gp-bio1

Continues draft PR #44 on gp-8tp1 from b96526817b34147b9f171482700061c0e310ef2f.
The explicit-target guard now gathers bindings with one GET per identity from
`common.session_identity_candidates(session_id)` before checking for an active
conversation match. This covers the session ID, environment session name, and
API-reported alias and session name. Unbound targets retain the same refusal
before any POST. The round-4 delta adds no other binding queries to update.

The new regression runs the default invocation (no `--session`) with a live
company turn. Each identity is the sole holder of the requested binding in
turn: the ordinary route posts to the requested conversation and thread. A
fifth case has no binding under any identity and verifies that neither route
posts. The Fable r2 comment on gp-bio1 is CLEAN and assigns no additional fixes.

Evidence for this round:

- round5-tests-python-red.txt: at b9652681 with only the five new cases added,
  the three name/alias cases fail with the expected explicit-target refusal;
  566 pass, including the ID-bound and unbound controls.
- round5-tests-python-green.txt: all 569 pass after the guard fix. Both runs use
  `env -u GC_TEMPLATE uv run --no-project --with pytest python -m pytest
  slack-full/tests -q`, and report the same two existing fork DeprecationWarnings.
- round5-tests-go-full.txt: Go 1.26.5 darwin/arm64; 1,186 top-level tests and
  1,842 test/subtest outcomes pass, zero failures/skips. Run from
  slack-full/adapter with `env -u GC_TEMPLATE
  CGO_CPPFLAGS=-I/opt/homebrew/opt/icu4c@78/include
  CGO_LDFLAGS=-L/opt/homebrew/opt/icu4c@78/lib go test -count=1 -json ./...`.
  Raw JSON is retained locally at /tmp/gp-bio1-go-full.jsonl.
- round5-change.diff: zero-context diff from b9652681 for the guard and tests.

All test requests are intercepted; no live Slack messages were sent. The
prescribed `/Users/tailor512/city/.gc/shims/toolchain/pnpm exec node --version`
reports v24.21.0; these suites run on Python/Go, and no unsupported-engine
warning occurred. Fetch/push use the explicit aphexcx/gascity-packs-prs URL.
The fetched fork2/main and local origin/main both resolve to
7621e7b6a7f1db0a69c181d33b8c4e03f34fbc7a, also the PR merge base.

The review sandbox again fails the unchanged
`TestTightenStorePermissions/setgid_bit_preserved_on_dir` case. The worker's
full suite passed; a focused outside-sandbox rerun also passes the top-level
test and all eight subtests (round5-tests-review-environment.txt).
Recurrence filed as pc_8039b75293bd, following pc_57ad64ab9acc.

Worker Codex STANDARD round 5 (gpt-6-astra, first attempt) found no actionable
regressions and independently passed all 569 Python tests and focused Go
tests. See round5-review-standard-1.txt. The mayor gate r3 and Fable read r3
follow READY; PR #44 remains draft for the founder merge decision.

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

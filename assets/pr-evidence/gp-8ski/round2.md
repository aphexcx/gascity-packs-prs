# Round 2: exact approval and usable permission

Round base: `275618ec36161c48133998569f44aa512d125326`.
Google acceptance remains **UNPROVEN**. All requests use local fakes.
The branch and draft PR continue from round 1 without a rebase.

| Requirement | State | Change |
| --- | --- | --- |
| A. The owner's whole reply is the code | DONE | Trim surrounding whitespace and compare without letter case. Refusals, questions, quotations, extra words, repeated codes, embedded codes and punctuation refuse without a permission request or write; approval remains unused. |
| B. No usable token, no spent yes | DONE | Prepare and validate the create before saving approval as used. The prepared call has no second local token-expiry check after consumption. A short-lived token leaves the durable request unused; the same yes succeeds with a good token. Consumption is still durable before the POST. |
| C. Plain skill wording | DONE | Explain that Google's acceptance has not been proven and tell the owner what a refusal said. No internal testing instructions in the invitation section. |
| D. Room name and id in the ask | DONE | The command's ask includes the display name and exact Google resource id; the skill and help require both. |
| E. No guessed cure for Google's refusal | DONE | HTTP 403 PERMISSION_DENIED and 400 FAILED_PRECONDITION report the actual status and an unknown cause. The README and answers table make no promise about organization settings. |

## Verification

The regression tests were committed first in `c291ce3b`. Against round 1
production, the focused reproduction exits 1: 4 failing top-level tests,
15 failing and 22 passing subtests, no skips. The unchanged exact-code
success row remains green; the lowercase row becomes green with the fix.

After the fix, the focused join suite exits 0: 23 top-level tests and
69 subtests pass, no skips:

```sh
env -u GC_BIN -u NEVER_SILENT go test ./internal/server -run '^TestJoin|^TestMissingPermissionChangesNothing$' -count=1 -json
```

Each regression also ran separately from the adapter module, using
`env -u GC_BIN -u NEVER_SILENT go test ./internal/server -run '^NAME$'
-count=1 -json`. For red runs, `-overlay` substitutes the three changed
production files from the round base, leaving the final tests in place.

| Name | Round-base result | Fixed result |
| --- | --- | --- |
| TestJoinRefusesWithoutTheOwnersYes | Exit 1; 14 failed, 13 passed subtests | Exit 0; 27 subtests pass |
| TestJoinShortLivedPermissionPreservesYes | Exit 1 | Exit 0 |
| TestJoinAskNamesRoomAndID | Exit 1 | Exit 0 |
| TestJoinSaysWhatHappened | Exit 1; 1 failed, 9 passed subtests | Exit 0; 10 subtests pass |

There are no skips in these runs. Each command executes one top-level test.
The baseline overlay is read-only; no branch switch or rebase was needed.

Python 3.13.13: the prescribed `make test PYTEST_DIRS='tests gchat/tests'`
reports 465 passed, 1 failed, 127 skipped and 261 passing subtests. The one
failure is the unchanged nested-make runner test inheriting `PYTEST_DIRS`
through make's override environment, as in round 1. With those overrides
cleared, the same selected runner reports **466 passed, 127 skipped,
261 passing subtests**, exit 0:

```sh
env -u GC_TEMPLATE -u MAKEFLAGS -u MFLAGS -u MAKEOVERRIDES -u PYTEST_DIRS -u PYTEST_ARGS -u PYTEST_RUNNER uv run --with pytest --with pyyaml --with jsonschema python3 -m pytest tests gchat/tests -q
```

The independent read's three `test_gastown_lint_findings.py` failures were
shown identically at its base and head; that file is outside this delta and
untouched. They do not reproduce in this lane's runs above.
Go `go1.27.3 darwin/arm64`: the full suite passes **22 packages, 715
top-level tests (713 passed, 2 skipped), and 1,288 passing subtests**:

```sh
env -u GC_BIN -u NEVER_SILENT go test ./... -p=1 -count=1 -timeout=30m -json
```

The skips are `TestRealGCContract` and `TestRealGCReplyReminder`, which
require a live gc binary. No load-sensitive row failed, including the
3,000-message catch-up scenario. `gofmt -l .` is empty and
`env -u GC_BIN -u NEVER_SILENT go vet ./...` passes. `staticcheck` is absent.
Pack validation `gc lint gchat` and `git diff --check` pass. No shell script
changed in round 2. The race run passes 2 packages, 150 top-level tests and
225 subtests, with no skips or race reports:

```sh
env -u GC_BIN -u NEVER_SILENT go test -race ./internal/server ./internal/chat -count=1 -timeout=5m -json
```

The full suite and race run exercise the production and test source reviewed
at `24d663743c06d62ac4f95834e4132d1e299e34c0`; subsequent edits only record
these receipts. Public-repository check: done, zero redactions required.
Raw logs and completion manifests remain outside the public branch.

## Changed command text

- Invalid approval: "The owner must reply with this room's code alone, nothing else."
- Unusable token: "The permission could not be used to join. Nothing changed; run it again with the same owner's yes."
- Ask: "Ask the owner in a direct message about room [quoted name] ([exact id]). To say yes, reply with this room's code alone, nothing else, before it expires in 24 hours."
- Generic requested result: "Ask the owner in a direct message to reply with this room's code alone, nothing else, before it expires in 24 hours."
- Still invited: "An invitation exists, but Google did not join this identity. The cause is not known yet."
- Refused create: "An invitation exists, but Google refused the join. Google answered HTTP [status] [status word]. The cause is not known yet."

The final help, skill invitation section and README setup paragraph carry
the same code-alone rule, ignoring letter case and surrounding whitespace.
The skill's owner ask names the room and its exact id, inviter, outside
participants and the effect of joining, then asks for the code alone.

## Review

The prior gate's one MAJOR and the independent read's matching MAJOR share
one class: mistaking a code mention for affirmative approval. Every form
is covered in `TestJoinRefusesWithoutTheOwnersYes`, including exact-code
success, with permission count, write count and durable consumption checked.
The read's MINOR 2 and 3 and NOTE 4 and 12 are addressed by B through E.
These findings are FIXED in `24d663743c06d62ac4f95834e4132d1e299e34c0`;
the read's lowercase NOTE 13 is also covered by A's case-insensitive rule.
The other prior notes retain their recorded dispositions and are unchanged.
This round's validated DEEP review by gpt-6-astra covers
`275618ec36161c48133998569f44aa512d125326` through
`24d663743c06d62ac4f95834e4132d1e299e34c0`: **CLEAN**, zero CRITICAL,
MAJOR, MINOR or NOTE findings. It read all ten changed files and traced
callers, authorization, token handling, persistence and error paths.
It ran no tests in its sandbox; the lane's receipts above supply validation.
Later changes contain evidence only. The independent gate and second-model
read for round 2 remain pending.

Ordinary outgoing and incoming message retention can still contain approval
codes. Approval records store only a digest, and the join audit has no code.
The prior startup-state fix and the permission-refusal classification remain.

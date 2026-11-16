# Room invitation acceptance — gp-8ski

UNPROVEN: all Google and key holder requests in this work use local fakes.
Google does not document that a self-create accepts an invitation. A proof
call must settle U1-U4 before this draft is considered for release.

## Answers modeled by the fake

| Read or create answer | Command result | Evidence |
| --- | --- | --- |
| Read: JOINED | already_member; no permission request or write | DOCUMENTED G1; resource shape G8 |
| Read: INVITED | proceed to permission/owner checks | ASSUMED U2; state name DOCUMENTED G1 |
| Read: NOT_A_MEMBER or missing membership (404 NOT_FOUND) | no_invitation | ASSUMED U3; state meaning DOCUMENTED G1 |
| Read: any other membership state | no_invitation | ASSUMED U2/U3; fail closed |
| Create self: JOINED | joined | ASSUMED U1; request shape DOCUMENTED G3/G4 |
| Create: 400 FAILED_PRECONDITION | refused_invited | ASSUMED U4; G10 documents the adder, not the invitee |
| Create: 403 PERMISSION_DENIED | refused_invited | ASSUMED U4; G10 documents the adder, not the invitee |
| Create: 404 NOT_FOUND | no_invitation | ASSUMED U3 |
| Create: still INVITED | refused_invited | ASSUMED U1 |
| Unrecognized read HTTP/status pair | google_unknown; HTTP and status word, nothing changed | ASSUMED U2/U3; fail closed |
| Unrecognized create HTTP/status pair | google_unknown; HTTP and status word, outcome unknown; retry reads membership first | ASSUMED U1/U3/U4; fail closed |
| Lost response/transport error | google_no_answer; retry begins with a read | ASSUMED U1-U4 |

Google references G1-G11 are the Membership resource, create, RPC
CreateMembershipRequest, patch, Chat authorization, delegated user
authentication, membership get, spaces list/search, create-members, and
Workspace domain-wide delegation setup documentation, respectively.
The key holder's ability to grant a new scope (U5) remains outside this repository.

## Delivery contract

| Item | State | Implementation |
| --- | --- | --- |
| 1. One verb, one exact room | DONE | Shell and service validate independently; duplicate JSON fields and unknown fields refuse; one self-create at most. |
| 2. Owner's yes | DONE | WAY 2: random ten-character code, 24 hours, digest only in the join request record, owner identity and DM checked against Google, saved use before write. Ordinary message text retains its existing storage. |
| 3. Candidate join | DONE against fakes | One isolated create function, read first, documented and assumed answers distinguished above. Real Google remains UNPROVEN. |
| 4. Idempotence and records | DONE | Repeat and lost-response cases read before writing; concurrent approvals serialize; every attempt audits time, room, message name, result only. |
| 5. Permission rollout | DONE | Separate one-scope mint only during join/check; no changes to standing credentials, ordinary health or notices. |
| 6. Existing commands | DONE | Status and spaces implementations and their tests are unchanged. |
| 7. Owner ask | DONE | One invitation, inviter name/address, outside participants, owner yes, direct message and code; no repeated ask after silence or refusal. |
| 8. Update behavior | DONE | README, changelog and help explain optional enablement and the unchanged default; no version change. |

## Verification receipts

[Named test receipts](named-tests.md) contain all 27 exact invocations, baseline
and implementation exit codes, and counts: 27 top-level tests, 55 subtests,
zero skips at both revisions. Twenty-three feature tests are red against the dispatched main production
and green at the implementation. Three existing-behavior pins and the fake
model test pass at both stages. The initial test/fake commit is `99e03320`;
the final implementation tested is `20d2103f4d50daf0df5387387a155339696d180f`,
against base production `68884bf15089e79230bc88c55b591033e3fe927f` with
the final tests and fake Google overlaid.

The Python join baseline was `make test PYTEST_DIRS='tests gchat/tests'
PYTEST_ARGS='-q -k join'`: 12 failures (including ten validation subcases),
one pass, 590 deselections. At the implementation the focused run has three
passes, ten passing subtests and 590 deselections. No behavior test was skipped.

Go: `go1.27.3 darwin/arm64`. Python: the Makefile-selected uv runner uses
Python 3.13.13, despite the shell's Python being 3.15.0 without pytest.
No tools were installed. `staticcheck` is absent and was not run.

`gofmt -l .` is empty; `env -u GC_BIN -u NEVER_SILENT go vet ./...`,
`sh -n gchat/commands/join.sh`, `shellcheck -x gchat/commands/join.sh`, and
`gc lint gchat` all exit zero. The README's old `packs/gchat` lint path was
corrected to the actual pack directory.

The required `make test PYTEST_DIRS='tests gchat/tests'` run reports one
unrelated runner failure: `test_make_test_names_the_runner_a_worker_would_otherwise_rediscover`
inherits that command-line directory override in its nested `make -n` and
therefore cannot see the default directories it expects. Its other results
are 465 passes, 127 skips, and 261 passing subtests. Neither that test nor the
Makefile changes in this work. With make overrides cleared, the same selected
runner and directories pass: 466 tests, 127 skips, 261 subtests, exit zero:

```sh
env -u GC_TEMPLATE -u MAKEFLAGS -u MFLAGS -u MAKEOVERRIDES -u PYTEST_DIRS -u PYTEST_ARGS -u PYTEST_RUNNER uv run --with pytest --with pyyaml --with jsonschema python3 -m pytest tests gchat/tests -q
```

Only summary receipts are published: raw transient test logs can include local
temporary paths or fake approval values, so they stay outside the PR.

## Specification references that moved

At the implementation, `NewSource` is at `app.go:559`, service routes at
`server.go:130`, `validSpace` at `server.go:435`, the spaces handler at
`server.go:1514`, `GetMessage` at `chat.go:289`, `ResourceDenied` at
`chat.go:603`, and inbound membership handling at `inbound.go:633` with the
join cursor set at line 647. The scope-list and key-holder source references
remain unchanged. The dispatch base supersedes the older base in the prose.

## Text inventory

Every command answer/refusal, shell validation string, help text, owner-ask
section and README setup/update paragraph is reproduced in [strings.md](strings.md).

## Review and amended contract

[Self-review dispositions](self-review.md) record the DEEP review's two MAJOR
and one MINOR findings, the reproductions, fixes and accepted scope ruling.
All product corrections are in `20d2103f4d50daf0df5387387a155339696d180f`.
The original review verdict remains BLOCK; it is not relabeled as a passed
gate. The independent gate and second-model read belong to the later handoff.

The bead's amendment 1 and ruling 2 are implemented. Only
`delegation_not_authorized` means the Workspace admin scope step.
`scope_not_allowed` means a box software update or enabling its permission
setting; other permanent codes are returned as `permission_error` and do not
send the admin to add a scope. All retain result `permission_missing`.

Approval records and join audits hold no plaintext code, but the existing
ordinary message records may. This is a named limit, accepted in ruling 2;
`TestJoinWorkflowKeepsCodesOutOfJoinRecords` exercises outgoing and incoming
messages to pin both sides of that boundary. Unknown write outcomes now say
they are unknown and a retry reads membership first. Unknown reads still say
nothing changed. Before ledger load, the service returns HTTP 503 with
`state_unavailable`, logs the refusal, and reports that it could not save the
attempt record; it never overwrites existing state with the initial empty ledger.

The pre-correction full Go run passed all 22 packages, 710 top-level tests,
1,268 subtests and two skips with the required invocation. The corrected head's
27 individual named tests (55 subtests, no skips), focused race run (21 tests,
55 subtests, no skips), and repeated Python suite (466 passes, 127 skips,
261 passing subtests) also pass.

The full Go suite also passes at corrected source commit
`20d2103f4d50daf0df5387387a155339696d180f`: 22 packages, 713 top-level tests,
1,274 subtests, two skips, zero failures. The skipped tests are the optional
real-gc contracts; `GC_BIN` and `NEVER_SILENT` were unset. None of the known
load-sensitive cases failed. Existing status and spaces tests pass unchanged.

```sh
# From gchat/adapter:
env -u GC_BIN -u NEVER_SILENT go test ./... -p=1 -count=1 -timeout=30m -json
env -u GC_BIN -u NEVER_SILENT go test -race ./internal/server -run '^TestJoin|^TestMissingPermissionChangesNothing$' -count=1 -timeout=5m -json
```

Both commands exit zero. Final `gofmt -l .` is empty and
`env -u GC_BIN -u NEVER_SILENT go vet ./...` exits zero. The remaining commits
update evidence only; the verified product source stays at the SHA above.

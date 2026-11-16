# DEEP self-review, round 1

Reviewer: gpt-6-astra through the installed validated review helper.
Reviewed SHA: `bd69c499486c036f465233bac71dc4464e67333a`.
Original verdict: **BLOCK**, two MAJOR findings and one MINOR finding.
The verdict is retained as issued; the changes and dispositions follow.
The mayor's independent gate and second-model read remain pending.

| Finding and class | Reproduction | Outcome |
| --- | --- | --- |
| MAJOR: persistence before state initialization | `TestJoinBeforeLedgerLoadPreservesExistingState`: invalid, check, desktop and unavailable cases all overwrote an existing sentinel ledger before the fix. | FIXED in `20d2103f4d50daf0df5387387a155339696d180f`. The application supplies its atomic loaded flag; no join action or audit save runs before it is true. Early requests return 503, log the refusal, and explicitly report that their ledger audit could not be saved. All four cases now pass. |
| MAJOR: code retention claim exceeded the new record boundary | `TestJoinWorkflowKeepsCodesOutOfJoinRecords` sends the ask through normal publish, processes the reply through normal inbound, then joins. The ordinary send/change records retain text; the join request and audit do not contain the code. | INTENDED existing message retention, accepted in the bead's ruling 2. The overly broad README claim is FIXED in `20d2103f4d50daf0df5387387a155339696d180f`. Approval records hold the digest; join audits contain no code. Ordinary message text may contain it. No inbound or ordinary retention change. |
| MINOR: uncertain mutations described as no change | `TestJoinAmbiguousWritesNeverClaimNothingChanged`: a lost response after the fake accepts the join and an unknown HTTP/status response both failed the truthful-outcome assertion. | FIXED in `20d2103f4d50daf0df5387387a155339696d180f`, as accepted in ruling 2. Unknown writes say the outcome is unknown and retry reads membership before another write. Unknown reads still say nothing changed. |

The same fix implements amendment 1: only `delegation_not_authorized`
gives the admin scope step. `scope_not_allowed` explains that the box needs
an update **or** the permission enabled. Other permanent codes retain result
`permission_missing`, identify the code in `permission_error`, and say that
the admin's permission step will not fix it. All seven code rows preserve
health, standing credentials and zero notices. This amendment's six changed
rows were red before the fix and green afterwards.

Focused post-fix command:

```sh
env -u GC_BIN -u NEVER_SILENT go test ./internal/server -run '^TestJoin|^TestMissingPermissionChangesNothing$' -count=1 -json
```

Exit zero: 21 top-level tests and 55 subtests, no skips. The startup and
ambiguous-write regressions each have a recorded red run before correction.
Every blocking class has a class-level test. No review finding remains
without a disposition; no pending gate is represented as passed.

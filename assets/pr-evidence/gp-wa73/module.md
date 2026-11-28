# gp-wa73 module runs

The commands of record are the verify skill's commands for gchat/adapter, `.claude/skills/verify/SKILL.md` on main. They were run through its `verify` function under GNU Bash 5.3, with Go 1.27.3 on darwin/arm64. Counts are terminal events of the `go test -json` stream; summarize.py is not committed.

Code and tests are at `bc0019e83625f4a5a9978a76127fdeb302f6c949`. The base is PR 107's round 5 head, `db4be95184fb4dfc4ad8dbe6e7dcac1563f8f111`. The base was run as the head's tree under a `go test -overlay` that:
- puts back `gchat/adapter/internal/inbound/coalesce.go` and `inbound.go` as `git show db4be951:` prints them, and
- removes `coalesce_retirement_race_test.go`.

Those three files are the whole difference in `gchat/adapter`.

| Tree | Command | Started (UTC), load | Exit | Packages | Top-level tests | Subtests |
| --- | --- | --- | ---: | --- | --- | --- |
| head bc0019e8 | `verify -C gchat/adapter go vet ./...` | 03:16:03Z, 49.73 | 0 | | | |
| head bc0019e8 | `verify -C gchat/adapter go test -count=1 -json -timeout=30m ./...` | 03:16:03Z, 49.73 | 0 | 25 pass, 1 skip | 982 pass, 2 skip | 49487 pass |
| base db4be951 | the same, with `-overlay <scratch>/base.json` | 03:31:08Z, 25.75 | 1 | 24 pass, 1 fail, 1 skip | 978 pass, 2 fail, 2 skip | 49481 pass, 1 fail |

The two skipped top-level tests are TestRealGCContract and TestRealGCReplyReminder in test/contract, as at round 5.

The head's totals against round 5's own record (`assets/pr-evidence/gp-49y9/round5/verification.md`: 980 top-level, 49482 subtests, at `-p=1`) are:
- +2 top-level tests: TestWindowFlushYieldsToAHistoryRetirementBeforeItsSave and TestRetryRefreshYieldsToAHistoryRetirementBeforeItsSave.
- +5 subtests: the flush test's five rows.

## The base's two failures, under load

The base's run failed TestRestartWithALedgerStillDeliversTheOutage and TestSubscriptionRefusalRetriesWithoutSignIn/404 in internal/app. Both failed the same way: `health did not become ready in 5s: {State:catching-up Reason:recovering retained history}`. Run alone at the base they pass (base-failures-alone.txt).

## A first run, before the last test commit

A first whole run at `143fbb7c` (the guards, before the fourth and fifth flush rows) overlapped two minutes of the lane's mutant runs and ended at load 48. It failed five tests outside this change:
- in internal/app: TestMissingTokenReplacementResumes, TestSenderFillCredentialRefusal and TestSubscriptionRefusalRetriesWithoutSignIn, each on a 5 s health deadline;
- in internal/inbound: TestLookupHoldCarriesCredentialKind and TestLookupHoldPreservesTokenError, with `space lookup failed: transient`.

All five pass alone at that head. TestMissingTokenReplacementResumes also failed alone once at the base (preliminary-failures-alone.txt). The run of record above is the head's second run. The review round's reviewer was running single tests of internal/inbound beside it.

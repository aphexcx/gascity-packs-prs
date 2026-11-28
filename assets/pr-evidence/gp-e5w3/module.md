# gp-e5w3 module runs

The commands of record are the verify skill's commands for gchat/adapter, from `.claude/skills/verify/SKILL.md` on main. They ran through its `verify` function under GNU Bash 5.3, with Go 1.27.3 on darwin/arm64. Counts are the terminal events of the `go test -json` stream.

The head is `01cc478c282b9e89b9ef724186ddd1acb48d4ded`, the last code commit. The base is main, `926f17912914d9470b4ef477cc65e9fb70792b56`. The base ran as a head tree under a `go test -overlay` that:
- puts back `gchat/adapter/internal/inbound/coalesce.go`, `coalesce_retry.go`, `history.go` and `gchat/adapter/internal/ledger/ledger.go` as `git show origin/main:` prints them, and
- removes `coalesce_refused_bound_test.go`.

Those five files are the whole difference in `gchat/adapter`. So the base run is main's module whatever head tree carried the overlay (it was a5a19dc8).

| Tree | Command | Started (UTC), load | Exit | Packages | Top-level tests | Subtests |
| --- | --- | --- | ---: | --- | --- | --- |
| head 01cc478c | `verify -C gchat/adapter go vet ./...` | 14:24:52Z, 8.13 | 0 | | | |
| head 01cc478c | `verify -C gchat/adapter go test -count=1 -json -timeout=30m ./...` | 14:24:52Z, 8.13 | 1 | 23 pass, 2 fail, 1 skip | 985 pass, 2 fail, 2 skip | 49524 pass, 1 fail |
| base 926f1791 | the same, with `-overlay <scratch>/base/base.json` | 13:30:54Z, 8.46 (20.33 at the end) | 1 | 24 pass, 1 fail, 1 skip | 984 pass, 2 fail, 2 skip | 49505 pass, 1 fail |

Every failure is a timing wait, and each passes alone:
- **The head, internal/inbound:** TestUnansweredPostOrder/pending, `unanswered_test.go:440: timed out before re-post` (a 3 s wait). Its processor has coalescing off and never reaches the retirement this change touches.
- **The head, internal/app:** TestTokenReloadDrainsPublishBeforeRebuilding, `app_test.go:421: publish did not start`.
- **Alone, head-failure-alone.txt:** each of the two passes three times at the head and three times at the base. One base run of TestUnansweredPostOrder took 3.78 s.
- **The base, internal/app:** TestObservedIdentityReplacesContradictedLedgerIdentity (`Client.Timeout exceeded while awaiting headers`) and TestShutdownCheckpointsBeforeDrainingStalledRequestBody/post-drain-save (`condition did not become true`). Alone at the base both pass (base-failures-alone.txt).

**Counts before and after.** Every test the base ran is in the head's run, under the same package and name, so none is removed. The head adds 20: TestHistoryBoundAcrossARefusedMergedTurn and its 19 rows.
- Top-level tests: 988 at the base and 989 at the head, with 2 skipped at each (TestRealGCContract and TestRealGCReplyReminder in test/contract).
- Subtests: 49506 at the base and 49525 at the head.

**Earlier head runs**, for the record:
- `a5a19dc8` (18 rows): exit 0. 25 packages pass, 1 skip; 987 top-level pass, 2 skip; 49524 subtests pass.
- `366655ed` (code as at bf14cbaf, 20 rows): one internal/app timing failure, TestSettingsReloadUpdatesWakePolicy, which passed alone three times at that head and three at the base.

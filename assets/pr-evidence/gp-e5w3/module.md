# gp-e5w3 module runs

The commands of record are the verify skill's commands for gchat/adapter, from `.claude/skills/verify/SKILL.md` on main. They ran through its `verify` function under GNU Bash 5.3, with Go 1.27.3 on darwin/arm64. Counts are the terminal events of the `go test -json` stream.

The head is `366655eda3a2409fd863acd51b930af3c1227415`. Its code is as at `bf14cbaf`; the commit after it adds the two review records only. The base is main, `926f17912914d9470b4ef477cc65e9fb70792b56`. The base ran as the head's tree under a `go test -overlay` that:
- puts back `gchat/adapter/internal/inbound/coalesce.go`, `coalesce_retry.go`, `history.go` and `gchat/adapter/internal/ledger/ledger.go` as `git show origin/main:` prints them, and
- removes `coalesce_refused_bound_test.go`.

Those five files are the whole difference in `gchat/adapter`.

| Tree | Command | Started (UTC), load | Exit | Packages | Top-level tests | Subtests |
| --- | --- | --- | ---: | --- | --- | --- |
| head 366655ed | `verify -C gchat/adapter go vet ./...` | 13:49:17Z, 11.17 | 0 | | | |
| head 366655ed | `verify -C gchat/adapter go test -count=1 -json -timeout=30m ./...` | 13:49:17Z, 11.17 | 1 | 24 pass, 1 fail, 1 skip | 986 pass, 1 fail, 2 skip | 49526 pass |
| base 926f1791 | the same, with `-overlay <scratch>/base/base.json` | 13:30:54Z, 8.46 (20.33 at the end) | 1 | 24 pass, 1 fail, 1 skip | 984 pass, 2 fail, 2 skip | 49505 pass, 1 fail |

Each run's failures are in internal/app, and each passes alone:
- The head: TestSettingsReloadUpdatesWakePolicy, `app_test.go:327: condition did not become true` (9.43 s). Alone it passes three times at the head and three times at the base, at about 1.3 s each (head-failure-alone.txt).
- The base: TestObservedIdentityReplacesContradictedLedgerIdentity (`Client.Timeout exceeded while awaiting headers`) and TestShutdownCheckpointsBeforeDrainingStalledRequestBody/post-drain-save (`condition did not become true`). Alone at the base both pass (base-failures-alone.txt).

Counts before and after: every test the base ran is in the head's run, under the same package and name, so none is removed. The head adds 21: TestHistoryBoundAcrossARefusedMergedTurn and its 20 rows. Top-level 988 at the base and 989 at the head, with 2 skipped at each: TestRealGCContract and TestRealGCReplyReminder in test/contract. Subtests 49506 at the base and 49526 at the head.

An earlier head run, at `a5a19dc8` (the code before the third round's two changes, with 18 rows), ended with exit 0: 25 packages pass, 1 skip; 987 top-level pass, 2 skip; 49524 subtests pass.

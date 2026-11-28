# gp-wa73 review round 1, at bc0019e8

A fresh reviewer on the lane's model (Claude Opus 5.5), as the mayor's sling comment asked. It had the diff, the ticket's scope and REVIEW.md from main, and none of the lane's conclusions. Its scratch folder is written `<scratch>` here.

**Verdict: CLEAN. CRITICAL 0 / MAJOR 0 / MINOR 0 / NOTE 5**

The reviewer's summary:
- Both guards are correct and complete for their saves.
- Removing either guard makes its own test fail for the reason the test's comment gives.
- `reflect.DeepEqual` is sound here:
  - every writer clones the ledger and assigns it back;
  - `slices.Clone` keeps nil and empty slices as they were;
  - `time.Time` values are copied bit for bit;
  - no writer rewrites a held or intent record on every pass, so neither loop can spin.
- Two scratch tests set an unrelated field (`Raised`) inside each gap. The guard trips, the next pass through the loop does the work, and exactly one turn is posted in the same pass.
- The tests are not vacuous: each checks that the retirement ran. The stack hook is deterministic: it passed 30 repeat runs and a `-race` run.
- Nothing private is in the diff, the test text or the three commit messages.

## Findings, with the lane's outcome

1. NOTE, coalesce.go:261-264. A guard trip skips `delete(p.recoverHeld, c.ID)`. The retired ids keep `flushWindows` (coalesce.go:342) running a snapshot every pass after coalescing is turned off, until a restart. Read, not run.
   Outcome: no change. This is the lane's self-review note 1.
2. NOTE, inbound.go:695. A guard trip still runs a full ledger `Save`, because `p.update` cannot give up. Read, not run.
   Outcome: no change. This is self-review note 2; main's `acceptingResult` saves behave the same way.
3. NOTE, coalesce_retirement_race_test.go:235. The refresh test covers only the nil-owner case. The comparison half is harmless: a scratch refresh test with the owner still present but changed gave `owner status=committed ... turns=1`.
   Outcome: no change. This is self-review note 4.
4. NOTE, commit 143fbb7c and coalesce.go:43-44. The wording "no field the merge or the refresh reads can be missed" overclaims for the refresh. The refresh's save works it out again from the live ledger, and reads members and message states that the owner comparison does not cover. Read, not run.
   Outcome: the pull request body says it precisely. The commit message stays, because history is not rewritten. The code comment at coalesce.go:43-48 describes the record gate only, and stays.
5. NOTE, coalesce_retirement_race_test.go:60. `guardedPoll` returns `(err error, panicked any)`, error first, against Go convention. Read, not run.
   Outcome: no change.

## The reviewer's shown runs

All were run from gchat/adapter with `env -u GC_BIN -u NEVER_SILENT`.

- No flush guard, by overlay: every flush row fails.
  - Rows 1, 3 and 4 panic: `nil pointer dereference (at ledger.(*ChangeState).OriginalKind ledger.go:224, called from inbound.(*Processor).flushSpace.func1 coalesce.go:237)` and `flushSpace.func1 coalesce.go:243`.
  - Row 2: `spaces/room/messages/m003: holds retired text "body-002"`.
  - Row 5: `turn spaces/room/messages/m003 posted retired text "body-002"`.
- No refresh guard, by overlay: `nil pointer dereference (at inbound.(*Processor).refreshMerged coalesce_retry.go:18, called from inbound.(*Processor).resume.func1.1 inbound.go:1315)`.
- `go test -count=1 -race` of the two new tests at HEAD: ok.
- `go test -count=30` of the two new tests: ok.
- 79 named tests from coalesce*_test.go, history*_test.go and retry_test.go: ok (98.6 s).
- The two scratch tests writing an unrelated field in each gap: both PASS.
- `gofmt -l` on the three changed files: no output.

Nothing was written in the repository and no git state was changed.

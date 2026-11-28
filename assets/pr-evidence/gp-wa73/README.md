# gp-wa73 evidence

PR 107's round 5 has a gap: a first read retires history under the ledger lock between a pass's snapshot and its save. This change adds two guards for it, one on the window flush's save and one on the retry refresh's save. It answers QUESTION 8 of the R14 to R19 ruling on PR 107's round 5.

Code and tests are at `bc0019e83625f4a5a9978a76127fdeb302f6c949`, stacked on PR 107's round 5 head `db4be95184fb4dfc4ad8dbe6e7dcac1563f8f111` (branch gp-49y9). Every command ran from the repository root through the verify skill's `verify` function, with GNU Bash 5.3 and Go 1.27.3. `<scratch>` stands for the lane's scratch folder outside the repository.

| File | What it shows |
| --- | --- |
| red.txt | The new test fails at round 5's code. It ran under an overlay that puts back coalesce.go and inbound.go as at db4be951, so the new test is the only difference from round 5's head. Rows 1, 3 and 4 and the refresh test panic with the probe's nil dereference, at coalesce.go:225 and :231 (the flush's save) and coalesce_retry.go:18 called from inbound.go:1310 (the refresh's save). Row 2 writes the merged pre-bound text over the redacted history records. Row 5 posts a redacted message's pre-bound text. |
| green.txt | The same two tests pass at the head. |
| mutants.txt | Each of five mutants fails at least one row. M1: no flush guard. M2: no refresh guard. M3: existence check only. M4: the flush checks only the owner. M5: a stale flush returns instead of picking again. |
| module.md | vet and the whole gchat/adapter module at the head (exit 0: 982 top-level tests pass, 2 skip; 49487 subtests pass), and the same command at the base. |
| base-failures-alone.txt | The base run's two load failures in internal/app pass alone. |
| preliminary-failures-alone.txt | A first, overlapped run's five load failures, alone at that head and at the base. |
| self-review-r1.md | The lane's self-review, round 1. |
| review-r1.md | Review round 1: a fresh reviewer on the lane's model, CLEAN (CRITICAL 0 / MAJOR 0 / MINOR 0 / NOTE 5), with the lane's outcome for each note. |

The rows of `TestWindowFlushYieldsToAHistoryRetirementBeforeItsSave` and the refresh test are in `gchat/adapter/internal/inbound/coalesce_retirement_race_test.go`. Each row's comment says what the retirement does and what the save did without the guard.

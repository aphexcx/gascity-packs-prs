# gp-e5w3 evidence

A merged turn's owner takes its first act's recorded time as its Created. When gc refused such a turn and a history bound fell between its acts, RetireSpaceHistory judged every act by that first time. It retired the acts recorded at or after the bound, and they were never posted. The Fable read of packs pull request 135 found it, as its finding 10 (probe P9), the same at main `926f17912914d9470b4ef477cc65e9fb70792b56`. This change makes a history bound retire only the acts recorded before it.

## How

The recorded time of each act is kept beside the turn's, and a turn that the bound divides is split back into its acts before the retirement.

- `ledger.ChangeState.MemberCreated` (`member_created`) is the owner's own recorded time. The window flush sets it when it gives the owner the turn's time. The refresh carries it when it moves a turn to a new owner. Members already kept their own Created. `OriginalCreated()` reads it, beside `OriginalEvent()` and `OriginalKind()`.
- RetireSpaceHistory first splits each turn that refreshMerged could still rewrite (never POSTed, or answered "not delivered") whose Created is before the bound and one of whose acts was recorded at or after it. `unmerge` gives each act back its own text, kind and recorded time as a single intent. The owner keeps the attempts gc refused. A creation with a folded edit keeps hold's kind "coalesced".
- An edit that the window folded into its message's creation gets its own record back, when the creation is before the bound and the edit is at or after it. Only an edit that no later revision superseded leaves the creation.
- The retirement then runs as before, with one comparison (`beforeBound`): an act recorded before the bound is retired, including one that was in a merged turn, and an act recorded at or after it stays an intent and is posted on its own.

Why this way: the retirement already judges a single record rightly, so the change gives it single records with their own times instead of teaching it about turns. A split needs no new merged text at retirement time. RetireSpaceHistory has no clock, and its signature is shared with pull request 135's tests. RetireQueuedHistory in the same file already returns a turn's members to single acts when a history retirement meets their owner. The kept owner's Created changes to its own time, so a pass that read the old turn before the split cannot POST its text: saveAttempt checks the Created it read. The cost: the kept acts of a split turn are posted as separate turns, not one merged turn.

A turn whose POST may have landed (an unanswered POST, a pending receipt, an unresolved turn) is not split. It is retired as before.

## A state file written by main

It has no `member_created`. A merged owner in it reads as recorded at the later of the turn's Created and its own act's time in Chat (`ActAt`). Both are at or before its recording. Every other record already carries its own recorded time in Created. An older adapter reading a state file written after this change ignores `member_created` and reads as before.

## Files

| File | What it shows |
| --- | --- |
| red.txt | TestHistoryBoundAcrossARefusedMergedTurn at main's code: the head's tree with the four changed code files as at main (an overlay), so the test file is the only difference from main. 15 of its 20 rows fail; the 5 that pass are the rows where main was already right. |
| green.txt | The same test at the head: 20 of 20 rows pass. |
| mutants.txt | 14 mutants of the new code, one overlay each. Every mutant fails at least one row (between 1 and 16). |
| mutants-diff.txt | Each mutant's one-line change. |
| module.md | Vet and the whole gchat/adapter module at the head and at main, with counts before and after. None removed, 21 added. |
| base-failures-alone.txt | The base run's two load failures in internal/app pass alone. |
| head-failure-alone.txt | The head run's one load failure in internal/app passes alone, three times at the head and three at the base. |
| review-r1.md, review-r2.md | The lane's two review rounds: fresh reviewers on the builder's model, and each finding's outcome. |

The records ran at `366655eda3a2409fd863acd51b930af3c1227415`, whose code is as at `bf14cbaf`. `<scratch>` stands for the lane's scratch folder outside the repository.

## The rows

The rows are in `gchat/adapter/internal/inbound/coalesce_refused_bound_test.go`. Each comment says what the row arranges.
- Where the bound falls: before the first act; at the first act's own time; between the acts; at the second act's own time (probe P9); after the last act.
- A restart between the refusal and the bound.
- The owner before the bound, with its member after it.
- A state file without the owner's time.
- Refreshes: to a later owner; to an owner that Chat dates before the bound; with the first act deleted after the refusal; with the first act edited after the refusal.
- An owner that Chat dates before the bound but that was recorded after it.
- Folded edits: one edit folded into a held creation; two edits folded; a folded edit that a later edit superseded; a kept owner with a fold; a fold inside a refused turn; a fold inside a turn whose POST may have landed.
- A turn whose POST may have landed.

## Commit history

The message of fac6df13 says five rows failed at main. Six did at that commit.

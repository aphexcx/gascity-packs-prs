# gp-e5w3 evidence

A merged turn's owner takes its first act's recorded time as its Created. When gc refused such a turn and a history bound fell between its acts, RetireSpaceHistory judged every act by that first time. It retired the acts that came at or after the bound, and they were never posted. The Fable read of packs pull request 135 found it, as its finding 10 (probe P9), the same at main `926f17912914d9470b4ef477cc65e9fb70792b56`. This change makes a history bound retire only the acts before it.

## How

The recorded time of each act is kept beside the turn's. Before the retirement, a turn that the bound divides is split back into its acts.

- `ledger.ChangeState.MemberCreated` (`member_created`) is the owner's own recorded time. The window flush sets it when it gives the owner the turn's time. The refresh carries it when it moves a turn to a new owner. Members already kept their own Created. `OriginalCreated()` reads it, beside `OriginalEvent()` and `OriginalKind()`.
- An act shows it came at or after the bound (`afterBound`) when it was recorded then and Chat dates it then too, if Chat gave it a time. An act recorded before the bound necessarily predates it. The pack judges each act by when it happened (gchat/README.md, "Catch-up and restart recovery"), and an event that came late is recorded after its act.
- `dividedAt` picks a turn whose Created is before the bound and one of whose acts came at or after it. The turn must be one that refreshMerged could rewrite (never POSTed, or answered "not delivered") and that has no fragment in a terminal.
- `unmerge` makes each act of such a turn a single intent again, with its own text and kind. An act that came at or after the bound takes back its own recorded time. Every other act keeps the turn's, so the retirement takes it as it took the whole turn. The owner keeps the attempts gc refused. Every kept creation that carries a folded edit keeps hold's kind "coalesced" for it.
- The retirement then runs as before, with one comparison (`beforeBound`). An act that came after the bound stays an intent and is posted on its own.

Why this way:
- The retirement already judges a single record rightly. The change gives it single records with the right times; it does not teach it about turns.
- A split needs no new merged text at retirement time. RetireSpaceHistory has no clock, and its signature is shared with pull request 135's tests.
- RetireQueuedHistory, in the same file, already returns a turn's members to single acts when a history retirement meets their owner.
- A kept owner's Created becomes its own time, so a pass that read the old turn before the split cannot POST its text: saveAttempt checks the Created it read.
- The cost: the kept acts of a split turn are posted as separate turns, not one merged turn.

A turn is not split, and is retired as at main, when:
- its POST may have landed: an unanswered POST, a pending receipt, or an unresolved turn;
- a fragment of it is in a terminal;
- no act of it shows it came after the bound.

## A state file written by main

It has no `member_created`. A merged owner in it reads as recorded at the later of the turn's Created and its own act's time in Chat (`ActAt`); both are at or before its recording. Every other record already carries its own recorded time in Created. An older adapter reading a state file written after this change ignores `member_created` and reads as before.

## Files

| File | What it shows |
| --- | --- |
| red.txt | TestHistoryBoundAcrossARefusedMergedTurn at main's code: the head's tree with the four changed code files as at main (an overlay), so the test file is the only difference from main. 10 of its 19 rows fail. The 9 that pass are where main was already right: every row whose acts all go with the turn, and the rows where nothing is retired. |
| green.txt | The same test at the head: 19 of 19 rows pass. |
| mutants.txt | 14 mutants of the new code, one overlay each. Every mutant fails at least one row (between 1 and 10). |
| mutants-diff.txt | Each mutant's one-line change. |
| module.md | Vet and the whole gchat/adapter module at the head and at main, with counts before and after. None removed. |
| base-failures-alone.txt | The base run's two load failures in internal/app pass alone. |
| head-failure-alone.txt | The head run's two load failures pass alone, three times each at the head and three at the base: one in internal/inbound (coalescing off, outside the retirement) and one in internal/app. |
| review-r1.md, review-r2.md | The lane's two review rounds: fresh reviewers on the builder's model, and each finding's outcome. |
| codex-r1.txt, codex-r1.md | The lane's Codex round 1 at 6640237a (the city gate script on the hub, its header without local paths), and each finding's outcome. |
| codex-r2.txt, codex-r2.md | The lane's Codex round 2 at a5258518, BLOCK 3/3, with each finding's status. No code changed after it: the lane asked the mayor (BLOCKED gp-e5w3). |

The records ran at `01cc478c282b9e89b9ef724186ddd1acb48d4ded`. `<scratch>` stands for the lane's scratch folder outside the repository.

## The rows

The rows are in `gchat/adapter/internal/inbound/coalesce_refused_bound_test.go`. Each comment says what the row arranges.
- Where the bound falls: before the first act; at the first act's own time; between the acts; at the second act's own time (probe P9); after the last act.
- A restart between the refusal and the bound.
- The owner before the bound, with its member after it.
- A state file without the owner's time.
- Refreshes: to a later owner; with the first act deleted after the refusal; with the first act edited after the refusal.
- Acts Chat dates on the other side of the bound from their recording: an owner Chat dates before the bound, from the start and after a refresh; an owner recorded before the bound that Chat dates after it, from the start and after a refresh.
- Kept creations carrying a folded edit, as the owner and as a member.
- A turn with a fragment in a terminal, and a turn whose POST may have landed.

## Left for the mayor

The lane stopped after the Codex round 2 (codex-r2.md) and asked the mayor which time decides that an act came after the bound, and whether a split turn should stay one delivery. Besides those questions:


These are the same at main unless the list says otherwise.
- **An edit folded into a creation before the bound.** The creation is retired, and the edit with it. The first two rounds released such an edit; codex-r1.md says why that came out again.
- **An unresolved merged turn across a bound** (review-r2.md finding 1, codex-r1.md finding 4).
- **A turn whose POST is in flight across a bound** (review-r2.md finding 2).
- **A second waiting mail** when a waiting mail is in flight during a split (codex-r1.md finding 7). This one is new with this change.

## Commit history

The message of fac6df13 says five rows failed at main. Six did at that commit.

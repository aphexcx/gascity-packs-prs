# gp-srhp: the Google Chat adapter's owner part

The adapter keeps no owner. On a box, root's copy of its owners file decides who is an owner. A code-shaped message is handed to root's door before the mayor sees it. `gc gchat owners` lists the owners and removes one after an owner's yes. `owner-claim-check` answers the box's helper.

- START: `926f17912914d9470b4ef477cc65e9fb70792b56` (origin/main of aphexcx/gascity-packs-prs at the claim; main had not moved at the last fetch).
- Code head: `2563f6dc0a350d5a8a766de413f3f7139a10cf49`. The next commit adds this folder only.
- `go version go1.27.3 darwin/arm64`. Python 3.12.13 for the pack's tests.
- The text that rules, in order: the contract (sha256 `41304ce9…`), the ticket and the mayor's rulings 1 to 18, then the rewritten spec as detail.

## Commits

| Commit | What |
|---|---|
| `146ad051` | The tests and the smallest declarations that let them compile (A1). One row panicked on an index here. |
| `e8270ad3` | That row fails on its assertion instead. **A1 is measured here.** |
| `951afdf3` | The implementation. |
| `f4e0368f` | The self-review's fixes. |
| `e27b2d03` | Codex round 1's fixes. |
| `0a6fe7d4` | Codex round 2's fixes. |
| `2563f6dc` | The test books wait one second for the stand-in door, not 200 ms (a row read no answer on a loaded machine). |

## The commands of record and their counts

The repository's verify skill at the base is the command of record. Its function runs every command; no make target covers this module. "Tests" counts every test and subtest event.

| Check | Command | Result |
|---|---|---|
| A0 | the spec's start checks ([a0.txt](a0.txt)) | PASS: START holds the spec's base, the two counts are 1 and 1, and nothing of this ticket's names existed |
| C-1 red (A1) | `verify -C gchat/adapter go test -count=1 -json -run 'ChatOwner\|JoinTeammate' ./...` at `e8270ad3` ([c1-red-a1.txt](c1-red-a1.txt)) | exit 1: 20 passed, 87 failed, no build error, no panic |
| C-1 green | the same at `2563f6dc` ([c1-green.txt](c1-green.txt)) | exit 0: 136 passed, 0 failed |
| C-2 | `verify -C gchat/adapter go vet ./...` | exit 0 |
| C-3 | `verify -C gchat/adapter go test -count=1 -json -timeout=30m ./...` | exit 1 at the code head: 50627 passed, 1 failed, 2 skipped. The one failure, `TestMissingTokenReplacementResumes` (internal/app), is a timing test that fails at START too and passes alone at the head; every run is in [c3-whole-module.txt](c3-whole-module.txt) |
| C-4 | `verify -C gchat/adapter go test -race -count=1 -json -run 'ChatOwner\|JoinTeammate' ./...` ([c4-race.txt](c4-race.txt)) | exit 0: 136 passed, 0 failed |
| C-5 | `verify -C gchat/adapter gofmt -l .` | prints nothing |
| C-6 | `verify python3 -m pytest gchat/tests -q -p no:cacheprovider` | 60 passed, 113 subtests passed |
| A4 | `git diff --numstat START 0a6fe7d4 -- gchat` ([a4.txt](a4.txt); `2563f6dc` changes only new test files and the fake) | PASS: the two test files that existed at START gained lines and lost none (`join_test.go` +5, `test_commands.py` +38) |
| STOP 2 | `git diff START HEAD -- gchat/adapter/go.mod gchat/adapter/go.sum` | empty |
| STOP 8 | `git diff --name-only START HEAD` | only `gchat/` and this folder |

Under load, one older test, `TestCommandPreparationFailureSettlesExistingIntent/not-ready`, once missed its deadline in a package run. It passed five times out of five alone, and in every C-3 run. It touches no owner code.

## A1: the rows that passed at the red commit, and why

All 20 pass by their nature at a base that never knocks and has no command:

- `TestChatOwnerClaimCheck`: no door, silent, refused, unavailable, outside the line form. The base binary has no `owner-claim-check`, so it already answers no (exit 2, nothing on stdout).
- `TestChatOwnerUntouchedMessages`: all twelve rows, and the parent test. The base never knocks.
- `TestChatOwnerWhereRootIsPresentOrAbsent/present_without_a_copy`: the stub has no owner.
- `TestChatOwnerOneFunctionInbound/present_without_a_trusted_copy`: the base resolver of that fixture names no owner.

## A2: each such row bites

Each scratch change was applied alone, on the fixed code, in a copy of the module, and never committed. The C-1 filter ran on that row's package, then the file was written back. Every row of the trust table also got one ([a2-scratch-changes.txt](a2-scratch-changes.txt)). In the final runs, every row fails under its change.

Five rows stayed green under the first change because a second rule also refuses them. Each was run again with both rules taken out, and then it failed:

- 16385 bytes: the read limit backs the stat check. Raising the limit itself makes the row fail.
- A missing key, and a record without since: the key count refuses them first. Two rows were added where only the named-key rule decides, and both fail without it. The second also needs the time rule taken out, since an absent `since` is empty and the time rule refuses it.
- From the identity: inbound marks the identity's own messages local first.

One first change did not compile (the time form); it was redone.

## Names chosen

- The package `internal/owners`: the one function `Book.Owner(id, address)`, and `Any`, `List`, `Present` and `NoteSettings`, all from one `view()`.
- The trust rule: `readTrusted` and `openDir`. The door's calls: `Take`, `Remove`, `Here`. The shapes: `CodeShaped` (C4) and `OneTimeCode` (the reaction provider's guard).
- The test stand-ins: `owners.ForTest(TestOptions)`, which panics outside a test binary, and the stand-in door `internal/fakes/fakedoor`.
- The route `POST /owners` and the command `gc gchat owners` (`gchat/commands/owners.sh`, `owners/command.toml`, `owners/help.md`).
- The state keys: `owner_requests` (by the Chat id to remove) and `owner_events` (taken, tries_ended, removed; the newest 500), plus the change field `fixed_line`.
- The log lines: `gchat owner claim`, `gchat owners file`, `gchat owners settings ignored`, `gchat owners`.

## Every place that asks "is this sender an owner"

Each one goes through `Book.Owner` (ruling 5):

- the message frame (`inbound.go`, again after the knock);
- the reaction frame;
- the live history frame (`history.go:375`);
- the stored history frame (`HistorySpeaker`, through `principal.New`'s default book);
- the join's "no owner is configured" (`Book.Any`);
- the join's and the removal's check of a yes, and again right before the one write or remove (`checkOwnerYes` → `JoinOwner` → `Speaker` → `Book.Owner`);
- `gc gchat owners` (`List`, `Present`, `Any`).

## The duplicate guard the knock sits behind

Ruling 17. At `926f1791`, `gchat/adapter/internal/inbound/inbound.go:974-979`: a change already recorded resumes its own delivery, and a message whose creation is already recorded returns. The knock comes after both, and before the frame and the record. A message whose turn was not recorded after the door acted knocks again, and its turn carries the fixed line of the door's second answer (`TestChatOwnerDeliveredTwice/not_recorded`). The cost: a newer claim can lose one try to it.

## The words added

Every sentence added to the README, the changelog, the skill and the command's help is in [words.diff](words.diff). The README says the three things of the ticket's scope 7: on a box, owners come from root's file alone; a city that is not a box keeps the owners its settings name and never takes a code; and adding is by the owner page alone.

## Fixtures that carry a premise (UNPROVEN)

- `gchat/adapter/internal/fakes/fakedoor/fakedoor.go`, the package comment: the real door answers in the contract's line form.
- `gchat/adapter/internal/server/owners_test.go`, `yesFrom`: Google gives every delivered message its creation time (U5).

## Uses of the lane's leave

- Files the spec's scope does not name, each with no change to what the adapter does for anyone else:
  - `internal/fakes/fakedoor/` (a fake);
  - `internal/config/config.go` (the reaction provider's guard);
  - `internal/inbound/coalesce.go` (a knocked catch-up turn is not merged);
  - `internal/server/server.go` (history leaves out the fixed line);
  - `internal/ledger/owners.go`.
- `gchat/tests/test_commands.py`: two tests added for the new command.
- `internal/server/join_test.go`: its fixture names its owners on an absent directory of its own (five lines added).

## Restore and reset with the new state

Neither changed. A restore installs the backup's `owner_requests` and `owner_events` as it installs the join's requests. A reset starts them empty. No owner, claim or try is in the state, so neither changes an owner (`TestChatOwnerRestoreResetBackup`).

## The ticket's acceptance checks, and the test of each

1. Knocked once, with the code, sender and name, before the hand-over: `TestChatOwnerTakeRows` (every row asserts the one line and that no turn reached gc first).
2. Room, other words, own account, edit: no knock (`TestChatOwnerUntouchedMessages`).
3. The saved state holds no owner, claim or try: `TestChatOwnerTakeRows` (each row reads the saved file), `TestChatOwnerStateKeys`.
4. With root's copy, an owner at every asking place; a settings-only owner is not, with one log line:
   - `TestChatOwnerOneFunctionInbound/present` (frame and reaction);
   - `TestChatOwnerJoinAsksTheOneFunction` (the join);
   - `TestChatOwnerOwnersRemove` (the removal's yes);
   - `TestChatOwnerWhereRootIsPresentOrAbsent/present`.
5. Link, owner, mode and directory refusals: `TestChatOwnerCopyTrustRule`.
6. No setting, variable or flag moves the path: `TestChatOwnerPathIsFixed`, `TestChatOwnerClaimCheckPathIsFixed`.
7. Root's directory absent: the settings, and no knock:
   - `TestChatOwnerUntouchedMessages/where_root's_directory_is_absent`;
   - `TestChatOwnerOneFunctionInbound/absent`;
   - `TestChatOwnerWhereRootIsPresentOrAbsent/absent`.
8. `owner-claim-check`: `TestChatOwnerClaimCheck`.
9. The list, and asked to add: `TestChatOwnerOwnersList`, `TestChatOwnerOwnersAdd`, `TestChatOwnerOwnersAddWhereRootIsAbsent`.
10. A removal: `TestChatOwnerOwnersRemove`, covering a good yes, self-removal, the last owner, and every refused yes sending the door nothing. Also `TestChatOwnerOwnersDoorDoesNotConfirm`.
11. Restore, reset and a hand-made backup: `TestChatOwnerRestoreResetBackup`.
12. The door stops answering:
    - owners stay while the copy is there (`TestChatOwnerTakeRows/no_answer_after_a_copy_naming_the_sender`, `TestChatOwnerWhereRootIsPresentOrAbsent/present`, which has no door);
    - no copy, no owner (`TestChatOwnerCopyReadAgainWhenChanged`, `.../present_without_a_copy`).
13. No log line or new state key carries the code, the text, a name or an address:
    - `TestChatOwnerTakeRows` (every row);
    - `TestChatOwnerOwnersLogLines`;
    - `TestChatOwnerCodesNeverReachTheReactionProvider`.
14. The README, read: "Where root's directory `/run/gchat-owner-claim` exists, owners come from root's file alone ...". Then "A city that is not a box has no root directory: its owners are the ones its settings name (`GCHAT_OWNERS`), as before, and no owner code is ever taken there." Then "A new owner comes by a one-time code from the owner page, and by nothing else."
15. The verify skill's commands above. The hosted rows are the mayor's, after the ready flip.

## Reviews

- [The self-review](self-review-r1.md): a fresh reviewer on the lane's model. 0 BLOCKER, 0 MAJOR, 3 MINOR, 11 NIT, each with its outcome.
- [Codex round 1](codex-r1-outcomes.md): BLOCK, 4 CRITICAL, 6 MAJOR, 4 MINOR; the fixes are in `e27b2d03`.
- [Codex round 2](codex-r2-outcomes.md): BLOCK, 3 CRITICAL, 10 MAJOR, 2 MINOR; the fixes are in `0a6fe7d4`.
- The lane stops its rounds at two. What is left is in the two outcome files, marked "For the mayor".

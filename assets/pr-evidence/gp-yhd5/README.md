# gp-yhd5: the session's reaction before the acknowledgement's record (PR 104 follow-up)

Stacked on PR 104 (branch gp-2awj at 972ff72c). Round 1's code tip 16f9d03e; round 2's 4d2099b5 (its own section below). Fakes only, nothing deployed, GC_BIN unset.

## The mechanism in five lines

1. When the channel answers that the session's reaction was placed (Slack ok, Google created), the react verb marks it on disk (`<state>/ack-session/<hash of conversation, message and emoji>`, synced). The reaction counts as it always has.
2. The session's "already there" counts as new only when the acknowledgement's placement record stands and no mark does. Taking the record also marks the reaction, so a marked reaction's "already there" is the session's own repeat, whether the record was written before or after the session reacted.
3. A removal the channel confirms (Slack ok or no_reaction, or a Google delete that found the reaction) clears the mark, so the session's next reaction counts as it would with the switch unset. NotePlacing is unchanged, and a placed answer leaves the record alone.
4. One mutex in ack.Service covers every read and write of records and marks, so of two "already there" answers at once, exactly one counts. Marks are pruned after two days and records after one, so pruning never leaves a record younger than its mark.
5. With ACK_REACTION unset, the session verbs return before touching disk, and Google Chat's hooks are not installed.

**Why not "no record while a mark exists"** (the first proposal): the session can remove its reaction after the mark and before the acknowledgement's call lands. The reaction there is then the acknowledgement's, with no record. The session's next reaction would get "already there" and not count, although with the switch unset the channel would place it and it would count. Keeping the record and gating the take on the mark covers that order (session-first/removed-then-acknowledged).

## Results

**Red at 79b7e39e** (the code of 972ff72c plus the two test commits):
- The session-first subtests fail with the acknowledgement on, in both adapters:
  - Slack lines 309/312 (lost), 350/353 (pending) and 373 (B's line);
  - Google Chat lines 136/139, 178/181 and 203.
- Every control with ACK_REACTION unset passes, and so does the removed-then-acknowledged guard.
- The one-taker test fails all 20 runs in both ack copies.

**The self-review's added Google Chat subtest** (removal-matched-nothing) is red on the code of 972ff72c and of cb597aad, at line 260.

**Mutations of the tip 16f9d03e** (red.txt, section 3):
- m1, no mark on a placed answer: the three session-first subtests (and removal-matched-nothing) fail.
- m2, a removal keeps the mark: the guard fails.
- m3, no lock: the one-taker test fails 20 of 20.
- m4, a mark lost on restart: only the two subtests that restart fail.
- m5, marks pruned at the records' age: the mark test fails.
- m6, Google Chat clears the mark on an empty removal: removal-matched-nothing fails.

**Green at 16f9d03e:**
- All of the above tests pass. Round 4's tests and the off-parity tests are unedited and pass. The two ack copies are identical.
- Full suites:
  - Slack: exit 0; 1677 top-level tests passed, 0 failed, 3 skipped.
  - Google Chat: exit 1; 722 passed, 1 failed, 2 skipped. The one failure is main's e2e 3000-message drain, which ran out of its time budget; run alone it passed 5 of 5 (321 to 385 s).
- The pytest target: 110 passed.

**Round 4's README sentence** ("A reaction from the session counts even when the adapter's acknowledgement placed the same one first; the acknowledgement alone never does.") is still true. It is unchanged.

## Round 2: the mark against the channel's order

Code tip 4d2099b5 on 3dc345a1. Fakes only, nothing deployed, GC_BIN unset.

The commits:
- 084fdc8f: the parity tests, red.
- f9a078fd: the fix.
- 53971345: the self-review's row, red.
- 4d2099b5: its fix.

**The review's finding.** The session's removal reaches the channel and its answer is still pending. The acknowledgement places the same emoji. The session reacts again, and the mark, not yet cleared, vetoes a reaction that counts with the switch unset. Google Chat also answered 502.

**The class.** The mark is the adapter's belief that the session's own reaction is on the message. It is written and cleared when the react verb handles a channel answer, and it is read to judge another call whose answer the channel decided at a different moment. When the channel applies two session calls on one reaction in one order and the adapter handles their answers in the other, the mark is stale:
- it vetoes a reaction that counts (cases 1 and 3);
- it is cleared late, so a later repeat counts (case 4);
- it is missing when a second identical add is judged, so both count (the overlap rows; round 1 listed this as a base limit).

**The mechanism.**
- `ack.Service.HoldReaction` gives the react verb one turn per conversation, message and emoji, keyed the same way as the mark.
- Both react verbs hold that turn across the channel call and its bookkeeping, for adds and removals alike. A second session call on the same reaction therefore reaches the channel only after the first call's answer is handled, so the mark follows the order in which the channel applied the session's calls. No new state is kept.
- A call on another message or emoji never waits, and the acknowledgement's own call takes no turn.
- With `ACK_REACTION` unset nothing is held, and Google Chat's hook is not installed.
- A call that waited for its turn is dated back by its wait in never-silent (the self-review's finding): the session acted when it asked, so a message that arrived during the wait is not answered by it. The wait is exactly zero when the turn was free, and always with the switch unset.

**The turn's bound.** A turn lasts as long as its holder's channel call:
- Slack: the react call is bounded by `slackAPIClient`'s 30 s timeout.
- Google Chat: a create is bounded by `CreateReaction`'s 30 s retry bound, a removal by `RemoveReaction`'s 30 s timeout.
- Either call ends sooner if the request's own context ends.

A call queued behind k others on the same reaction waits at most k of these bounds. A turn is dropped with its last caller, so nothing is kept.

**Why the acknowledgement takes no turn.** With a turn, the acknowledgement would wait behind a session call whose answer is held, which is the review's own sequence. Mutation m3 shows rows stalling 25 to 30 s and the guard `session-add-while-ack-answer-held` failing.

### The test (red first, in a commit of its own)

`TestHeldAnswerParityWithAckOff`, one per adapter (`slack-full/adapter/held_answer_parity_test.go`, `gchat/adapter/internal/app/held_answer_parity_test.go`), on round 4's rigs:
- A front stub (Slack) or proxy (Google Chat) applies every call and can hold its answer: the session's add, the session's removal, or the acknowledgement's add.
- Each row runs with `ACK_REACTION=eyes` and again with it unset. These must be equal between the two runs:
  - the receipts (Slack) or status codes (Google Chat);
  - the acts at each settle;
  - the journal's session_react rows since the last settle, compared as a set (Slack);
  - the owner lines never-silent posts three minutes later with the session down.
- Each row also checks the order in which the channel applied the calls, so a row cannot pass without reaching its order.
- A call that waits behind another call on its reaction is waited for 500 ms (Slack) or 750 ms (Google Chat), and then the row goes on.

| row | steps | before the fix: switch on vs unset | at 4d2099b5 |
|---|---|---|---|
| case1/removal-applied-answer-pending | add s1; remove r1 held; ack; add s2; release r1 | at 3dc345a1, s2 not counted: acts 1 vs 2 (Google Chat: 502) | equal |
| case3/add-and-removal-answered-in-the-other-order | add s1 held; remove r1; release s1; ack; add s2 | at 3dc345a1, s2 not counted: acts 1 vs 2 (Google Chat: 502) | equal |
| case4/repeat-after-the-held-removal-lands | case 1, then add s3 | at 3dc345a1, s3 counts where unset counts nothing | equal |
| overlap/identical-adds-answered-in-the-other-order | ack; remove r0; add s1 held; add s2; release s1 | at 3dc345a1, both count: acts 2 vs 1 | equal |
| overlap/session-add-pending-while-ack-answer-pending | add s1 held; ack held; add s2; release s1; release ack | at 3dc345a1, both count: acts 2 vs 1 | equal |
| stamp/message-arrives-while-a-call-waits | case 1 with message B admitted while s2 waits | at f9a078fd, B is answered by s2: owner lines 0 vs 1 | equal |
| guard/session-add-while-ack-answer-held | ack held; add s1 (prompt); release ack; add s2 | equal | equal |
| guard/other-emoji-and-message-not-held | add s1; remove r1 held; thumbsup on A and eyes on B (prompt); release r1; ack | equal | equal |

Two more tests:
- `TestReactVerbHoldEndsWithTheCallsTimeout`, in each adapter: a removal whose answer never comes holds its reaction only until its own call ends (a 300 ms client timeout on Slack, a 300 ms request deadline on Google Chat). The session's next call then goes through while the answer is still held.
- `TestHoldReactionIsOneTurnPerReaction`, in both ack copies:
  - the same reaction in another spelling waits;
  - two waiting calls go one at a time;
  - another emoji, message or conversation does not wait;
  - the wait it reports is zero when the turn was free and at least the time spent waiting otherwise;
  - nothing is kept after the last release;
  - with the switch off, nothing waits.

### Results

**Red** (r2-red.txt):
- The code of 3dc345a1 with the final test file: the six red rows fail in both adapters (held_answer_parity_test.go:419 on Slack, :435 on Google Chat). The two guards and the bound test pass.
- At 53971345 (the code of f9a078fd): only the stamp row fails, in both adapters.

**Mutations of 4d2099b5** (r2-red.txt). Each fails in both adapters:
- m1, no turn: the six red rows fail, and so does the unit test.
- m2, the turn ignores the emoji: the other-emoji guard and the unit test fail.
- m3, the acknowledgement takes a turn: its guard fails, and four rows stall.
- m4, turns kept after release: the unit test fails.
- m5, a turn with the switch off: the unit test fails.
- m6, the act not dated back: the stamp row fails.

**Green at 4d2099b5** (r2-green.txt):
- Every row and test above passes.
- Round 1's and round 4's tests and the off-parity tests pass. No earlier test file was modified: the git diff against 3dc345a1 lists only the four added files.
- The two ack copies are identical.

**-race -count=20 at 4d2099b5:** 20 of 20 in both adapters for each of these:
- the parity table;
- the bound test;
- round 1's two-goroutine test `TestPlacementRecordHasOneTaker`;
- `TestHoldReactionIsOneTurnPerReaction`.

No data race was reported.

**Full runs at 4d2099b5:**
- Slack: `env -u GC_BIN go test ./... -count=1` exits 0. 5 packages ok; 1680 top-level tests passed, 0 failed, 3 skipped. `go vet` is clean. `gofmt -l .` lists only `company_thread_ambient.go`, which is unformatted on main and not touched here.
- Google Chat: `env -u GC_BIN go test ./... -count=1 -timeout=30m` exits 0. 25 packages ok; 726 top-level tests passed, 0 failed, 2 skipped. `gofmt` and `go vet` are clean. The longer timeout gives main's e2e 3000-message drain its budget (round 1's note).
- The pytest target (round 1's command): 110 passed.

### Outcomes: the review's finding, the check's four cases, the self-review

- **The review's MAJOR** (stale session marks suppress valid reactions after removal): FIXED in f9a078fd. The row case1/removal-applied-answer-pending is red at 3dc345a1 and green at 4d2099b5 in both adapters.
- **Case 1** (a removal applied, its answer pending): FIXED, by the same row. s2 waits for r1's answer, and the trace equals the unset run's.
- **Case 2** (a removal applied, its answer lost or failed): INTENDED, stated under Limits. The self-review's probe shows it still open as stated (Slack acts 1 vs 2; Google Chat 502 vs 200).
- **Case 3** (an add and a removal overlap, answered in the other order): FIXED. The case3 row is red at 3dc345a1 and green at 4d2099b5.
- **Case 4** (case 1 once the held answer lands): FIXED. The case4 row is red at 3dc345a1 (the later repeat counted) and green at 4d2099b5.
- **Self-review, MINOR** (a call that waited was stamped when its wait ended, so it answered a message that arrived meanwhile): FIXED. 53971345 holds the red row, 4d2099b5 the fix.
- **Self-review, NOTE** (the table does not prove the bookkeeping happens inside the turn): DOES NOT MATTER. The turn is taken at the top of each verb and released by a defer when the verb returns, so the bookkeeping is inside it by construction. Pinning it would need a production seam between the answer and the bookkeeping.
- **Self-review, NOTE** (a waiting call ignores its request's context): DOES NOT MATTER.
  - Slack's react call already runs on a background context at the base.
  - Google Chat's call takes the request's context, so a call whose request has ended fails as soon as its turn comes.
  - The wait itself is bounded as stated above.
- **Self-review, NOTE** (this README was not yet updated): FIXED in this commit.

## Limits (stated, not fixed)

- **The session's own answer lost** (already in the base): the session's reaction lands but its answer is lost, it retries, and it gets "already there" while an acknowledgement record stands. The retry counts once; with the switch unset it counts 0. Nothing in the answers tells the two cases apart.
- **A placed answer no longer uses up the record.** Taking the record on a placed answer would lose the answer in the add, remove, acknowledgement-lands order above, which needs no fault. Keeping the record widens the first limit to one more two-fault sequence.
- **Slack emoji aliases** (`+1` / `thumbsup`): records and marks key on the name as sent. Out of scope here.
- **A removal whose answer is lost or fails** (round 2's case 2): the mark stays, so if the acknowledgement then places the same emoji, the session's next reaction of it is not counted where with the switch unset it counts, and Google Chat answers 502 to it and to each repeat until the session retries its removal, which clears the mark. It is left because it needs a fault on top of the session-first order and errs toward a notice never-silent would not otherwise post, never a hidden failure; closing it would mean clearing the mark when the acknowledgement's own add is placed, which is safe only if that add takes the reaction's turn (tying the acknowledgement's two-second budget to the session's calls: m3 shows it stalling 25 to 30 s), and on Google Chat a created answer does not prove the session's reaction is gone (Google may hold the emoji in another form), so the clear could over-count.
- Round 1's limit on overlapping identical session requests is closed by round 2's turn (the overlap rows).
- The mark directory is pruned at start and daily whatever the switch is set to, as `ack-attempts` and `ack-placed` already were.

## Files

- **red.txt:** the red runs, the added subtest on older code, and the mutations.
- **green.txt:** the tip's runs, the full suites' counts, main's load-sensitive tests run alone, and the pytest target.
- **r2-red.txt** (round 2): the parity table on the code of 3dc345a1 and at 53971345, and the mutations of 4d2099b5.
- **r2-green.txt** (round 2): the tip's runs, -race -count=20, the full suites' counts and the pytest target.

Paths are written `<clone>` (the lane's clone), `<scratch>` / `<scratch-NAME>` (a git archive, never committed) and `<tmp>`.

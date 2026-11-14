# gp-yhd5: the session's reaction before the acknowledgement's record (PR 104 follow-up)

Stacked on PR 104 (branch gp-2awj at 972ff72c). Code tip 16f9d03e. Fakes only, nothing deployed, GC_BIN unset.

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

## Limits (stated, not fixed)

- **The session's own answer lost** (already in the base): the session's reaction lands but its answer is lost, it retries, and it gets "already there" while an acknowledgement record stands. The retry counts once; with the switch unset it counts 0. Nothing in the answers tells the two cases apart.
- **A placed answer no longer uses up the record.** Taking the record on a placed answer would lose the answer in the add, remove, acknowledgement-lands order above, which needs no fault. Keeping the record widens the first limit to one more two-fault sequence.
- **Slack emoji aliases** (`+1` / `thumbsup`): records and marks key on the name as sent. Out of scope here.
- **Overlapping identical session requests** whose answers are handled out of order can still both count. This is a base property.
- The mark directory is pruned at start and daily whatever the switch is set to, as `ack-attempts` and `ack-placed` already were.

## Files

- **red.txt:** the red runs, the added subtest on older code, and the mutations.
- **green.txt:** the tip's runs, the full suites' counts, main's load-sensitive tests run alone, and the pytest target.

Paths are written `<clone>` (the lane's clone), `<scratch>` / `<scratch-NAME>` (a git archive, never committed) and `<tmp>`.

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

## Round 3: a waiting call ends with its caller; the stopwatch keeps its own time

Code tip 2486bb00 on 6a6e7ed7. Fakes only, nothing deployed, GC_BIN unset.

The commits:
- 4b0830f2: the waiter tests, red.
- 076cb06d: the stopwatch test, red.
- 86bc90e9: the fix for a waiting call.
- d2fb8f10: the stopwatch's fix.
- fba54c18: the dating back's false-notice row (the second read's note).
- 2486bb00: the self-review's wording.

**The review's MAJOR.** With `ACK_REACTION` on, a react call waiting for its turn on a reaction took no account of its caller. Its sequence: an eyes reaction placed; a removal whose channel answer is held; another eyes through the session's command, whose own timeout ends while the add waits. On Slack the abandoned add then reached Slack and counted. On Google Chat its handler was kept until its turn and then answered 502 with no call.

**The class.** A queue that cannot be left stands ahead of the channel call. Its cases in round 2's code:
- Slack: a waiting call is sent after its caller gave up, and counts.
- Google Chat: a waiting call's handler is kept until its turn.
- Either: every call queued behind a slow holder takes that holder's bound, whether or not its caller is still there.

The test at the class level cancels the waiter while the holder's answer is held.

**The mechanism.**
- The turn on one reaction is a channel with room for one token. A call takes the turn by putting the token in. A call that finds it full waits on it in a select with the react verb's request context, and waiting calls are served in the order they came.
- A call whose context ends while it waits leaves the queue at once with the context's error, holding nothing, and the calls behind it move up.
- A call checks its context once more after it has the turn, whether it waited or not: the turn and the end of the context can come together. So no channel call is made for a caller that has gone.
- The verbs answer such a call `{"delivered":false,"failure_kind":"transient"}` (Slack) or 502 (Google Chat) and record nothing: no act, no journal row, no mark.
- A call that holds the turn runs as before. Slack's react call still runs on a background context, bounded by `slackAPIClient`'s 30 s timeout.
- With `ACK_REACTION` unset nothing is held, nothing waits and nothing is refused. `HoldReaction` stays, as the same turn for a caller that never gives up.

**The caller's context.** It ends when the caller stops waiting, as a cancellation. It carries no deadline value of its own.
- Slack: the request's context on the adapter's internal listener (`main.go`, `internalSrv`; only `ReadHeaderTimeout` is set). The session's react command gives up after 10 s (`slack_intake_common.py`, `urlopen(req, timeout=10)`) and closes its connection to the gc API. gc's service proxy is a plain `httputil.ReverseProxy`, which sends the upstream request on the incoming request's context, so it closes the adapter's connection and net/http cancels the handler's context. The context also ends when the handler returns.
- Google Chat: the request's context on the app's server (`app.go`), whose base is the service's context. It ends when the session's command gives up (`commands/common.sh`, curl `--max-time 45`, through the same proxy) or when the service stops. The server's 25 s `ReadTimeout` does not end it while the handler runs (a probe of net/http).
- The proxy row runs this path in each adapter's test: an HTTP client with a 600 ms timeout, a reverse proxy, and the adapter on a unix socket.

**The review's MINOR: the stopwatch.** On Slack the act dated back by its wait also fed the stopwatch, so a session reaction that waited was recorded as landing earlier by the whole wait. The act now carries two times, as Google Chat's verb already keeps them:
- `Timestamp`, dated back, is what never-silent judges by and what the journal keeps for a restart's replay.
- `landed`, never persisted, is when the reaction completed. Only the stopwatch reads it.
- A call that did not wait gives both the same instant, as before.

### The tests (red first, in commits of their own)

- `TestReactVerbWaiterEndsWithItsCaller`, one per adapter (`slack-full/adapter/waiter_gives_up_test.go`, `gchat/adapter/internal/app/waiter_gives_up_test.go`), on round 2's holding rigs, with `ACK_REACTION=eyes`:
  - The steps: the session's eyes on A (s1); its removal (r1), applied and its answer held; the waiter (w) and a third eyes (s3) queued behind r1; w's caller gives up; r1's answer is released; a later eyes (s4).
  - Three rows, one for each way the caller gives up: its context is cancelled; its context reaches its deadline; its HTTP client times out through a reverse proxy in front of the adapter on a unix socket.
  - It asserts:
    - w returns within 2 s of its caller giving up, while r1's answer is still held;
    - by then the channel has applied only s1 and r1, and the acts, the journal (Slack) and the state files are unchanged;
    - the whole trace equals the same steps without w: the channel's applied order, the acts, the journal (Slack), the receipts or status codes of s1, r1, s3 and s4, and the state files;
    - s4 returns at once, so the turn is not kept;
    - w's caller got the transient receipt or 502, or, in the proxy row, its own timeout.
- `TestReactVerbWaiterGivenUpIsAMissedCount`, one per adapter: the parity limit's direction, below.
- `TestReactVerbStopwatchKeepsTheRealTimeOfAWaitedReaction`:
  - On Slack, red: a session eyes that waits behind its own held removal is the first reaction on its message. Its stopwatch line must lie between the release and the return. Its never-silent act must be dated back before the release, and the journal row must carry the never-silent time.
  - A guard: a call that did not wait gives one instant to all three.
  - Google Chat's copy is a guard, green before and after.
- `TestHeldAnswerParityMessageBeforeTheWaitingCall`, one per adapter: round 2's oracle on the row "add s1; remove r1 held; ack; message; add s2; release r1; settle". B is admitted before s2 asks, so s2 must not be dated back past its ask. Round 2's table file is unedited.
- `TestHoldReactionContextEndsWithItsCaller`, in both ack copies:
  - a waiter whose context is cancelled or reaches its deadline returns that error at once, holding nothing;
  - the calls queued before and after it go in order;
  - a caller already gone when the turn is free takes nothing;
  - nothing is kept, and a later call takes the turn at once;
  - with the switch off, a gone caller is not refused.

### Results

**Red** (r3-red.txt), this round's test files on the code of 6a6e7ed7:
- Slack: the three waiter rows fail. The waiting call had not returned 2 s after its caller gave up, with the holder's answer still held (waiter_gives_up_test.go:252); the trace differs from the run without it, with one more add applied by Slack and one more journal row (:259); and its caller was answered `{"delivered":true}` (:262). The limit test fails: the waiter counted. The stopwatch test fails: the line is at +72 ms from the ask, the release at +700 ms and the return at +899 ms. Its did-not-wait guard and the false-notice row pass there.
- Google Chat: the three waiter rows and the limit test fail on the prompt return (:234, :306), because the handler was kept until its turn. Google got no call for it at the base either, which is the second read's observation. The stopwatch guard and the false-notice row pass there.
- The ack unit test does not build at the base in either copy: `HoldReactionContext` is new.

**Mutations of 2486bb00** (r3-red.txt):
- r3m1, the wait ignores the caller: the waiter test, the limit test and the unit test fail in both adapters.
- r3m2, no check after the turn: the unit test fails in both copies (a caller already gone when the turn is free).
- r3m3, a waiter that gives up keeps its place: the unit test fails in both copies (callers and turns kept).
- r3m4, Slack's verb sends anyway: the waiter test and the limit test fail.
- r3m5, Google Chat's verb sends anyway: SURVIVES, as an equivalent mutant. Google Chat's create and removal take the request's context themselves, so a given-up call fails before any request to Google either way, with the same 502 and nothing recorded. The verb's check keeps that from depending on the client.
- r3m6, Slack's stopwatch reads the dated-back time: the stopwatch test fails.
- r3m7, the act dated back by twice its wait (the second read's own mutation): the false-notice row fails in both adapters. On Slack round 2's table and the stopwatch test fail too; on Google Chat the stopwatch guard does.

**Green at 2486bb00** (r3-green.txt):
- Every test above passes, and so do round 2's parity table and bound test.
- Round 1's and round 4's tests and the off-parity tests pass.
- No earlier test file was modified: the diff against 6a6e7ed7 and against PR 104's head 972ff72c lists no modified test file, and this round adds eight.
- The two ack copies are identical.

**-race -count=20 at 2486bb00:** 20 of 20 in both adapters for each of these: the four new app-level tests, round 2's parity table and its bound test, round 1's two-goroutine test `TestPlacementRecordHasOneTaker`, round 2's `TestHoldReactionIsOneTurnPerReaction` and the new `TestHoldReactionContextEndsWithItsCaller`. No data race was reported.

**Full runs at 2486bb00:**
- Slack: `env -u GC_BIN go test ./... -count=1` exits 0. 5 packages ok; 1685 top-level tests passed (round 2's 1680 and this round's five), 0 failed, 3 skipped. `go vet` is clean, and `gofmt -l` is clean on the files this round touched.
- Google Chat: `env -u GC_BIN go test ./... -count=1 -timeout=30m` exits 0. 25 packages ok; 731 passed (726 and five), 0 failed, 2 skipped. `go vet` and `gofmt` are clean.
- The pytest target (round 1's command): 110 passed.

### Outcomes: the gate's two findings, the second read's six, the self-review

- **Gate r2, MAJOR** (waiting reactions cannot be cancelled): FIXED in 86bc90e9. `TestReactVerbWaiterEndsWithItsCaller` is red at 6a6e7ed7 in both adapters and green at 2486bb00, in each of the three ways a caller gives up. The class's cases: the Slack send after the caller gave up (the extra add in the trace), the Google Chat handler kept (the prompt return), and the queued bound (the waiter leaves at once, and the calls behind it move up).
- **Gate r2, MINOR** (Slack's stopwatch early by the wait): FIXED in d2fb8f10. Red at 6a6e7ed7 (+72 ms against a release at +700 ms), green at the tip.
- **Read r2, MINOR 1** (the gate's MAJOR, graded MINOR there): FIXED, as above.
- **Read r2, MINOR 2** (the gate's MINOR): FIXED, as above.
- **Read r2, NOTE 3** (Google Chat's table does not pin the dating back from the false-notice side): FIXED in fba54c18. The read's row runs as its own test in each adapter, and r3m7 fails it in both.
- **Read r2, NOTE 4** (case 2 on Google Chat has no time limit short of the retry): FIXED in this commit. The Limits sentence now says so.
- **Read r2, NOTE 5** (the dating back was outside round 2's four scope items): DOES NOT MATTER. This round's contract builds on it (its scope item 4), and the read found it needed and sound.
- **Read r2, NOTE 6** (bead ids in the public delta): INTENDED. They are the branch names and the trailers the contract orders, and there is no other id.
- **Round 2's self-review NOTE** (a waiting call ignores its request's context, graded DOES NOT MATTER there): superseded. It is the gate's MAJOR, fixed above.
- **Self-review r3** (one Opus reviewer on this round's delta: BLOCKER 0, MAJOR 0, MINOR 1, NIT 3):
  - MINOR, the caller's context carries no deadline value of its own: stated above under "The caller's context". It ends when the caller gives up, which is what the wait needs, and the proxy row shows it.
  - NIT, the check after the turn also drops a call that did not wait, and the log line said "while it waited": the wording is FIXED in 2486bb00. The behaviour is the contract's (scope item 2) and is stated in the limit below.
  - NIT, Google Chat's comment did not say that a call holding the turn runs as before: FIXED in 2486bb00.
  - NIT, the evidence was not yet in the delta: FIXED in this commit.

## Limits (stated, not fixed)

- **The session's own answer lost** (already in the base): the session's reaction lands but its answer is lost, it retries, and it gets "already there" while an acknowledgement record stands. The retry counts once; with the switch unset it counts 0. Nothing in the answers tells the two cases apart.
- **A placed answer no longer uses up the record.** Taking the record on a placed answer would lose the answer in the add, remove, acknowledgement-lands order above, which needs no fault. Keeping the record widens the first limit to one more two-fault sequence.
- **Slack emoji aliases** (`+1` / `thumbsup`): records and marks key on the name as sent. Out of scope here.
- **A removal whose answer is lost or fails** (round 2's case 2): the mark stays, so if the acknowledgement then places the same emoji, the session's next reaction of it is not counted where with the switch unset it counts, and Google Chat answers 502 to it and to each repeat until the session retries its removal, which clears the mark; no timer ends it sooner. It is left because it needs a fault on top of the session-first order and errs toward a notice never-silent would not otherwise post, never a hidden failure; closing it would mean clearing the mark when the acknowledgement's own add is placed, which is safe only if that add takes the reaction's turn (tying the acknowledgement's two-second budget to the session's calls: m3 shows it stalling 25 to 30 s), and on Google Chat a created answer does not prove the session's reaction is gone (Google may hold the emoji in another form), so the clear could over-count.
- **A react call whose caller gives up before it has its turn** (round 3): with `ACK_REACTION` set, a react call that waits longer than its caller's own timeout is never made, where with it unset it would have been made at once. It errs toward a missed count, so never-silent may post a notice the unset run does not, never toward a hidden failure. `TestReactVerbWaiterGivenUpIsAMissedCount` shows it in both adapters: acts 1 against 2, owner lines 1 against 0. The session sees its own command's timeout: on Slack the react command raises `TimeoutError` after 10 s, on Google Chat curl stops at 45 s. The adapter's answer, `transient` (Slack) or 502 (Google Chat), reaches only a caller still listening. The same holds for a caller already gone when its turn is free, a window of microseconds.
- Round 1's limit on overlapping identical session requests is closed by round 2's turn (the overlap rows).
- The mark directory is pruned at start and daily whatever the switch is set to, as `ack-attempts` and `ack-placed` already were.

## Files

- **red.txt:** the red runs, the added subtest on older code, and the mutations.
- **green.txt:** the tip's runs, the full suites' counts, main's load-sensitive tests run alone, and the pytest target.
- **r2-red.txt** (round 2): the parity table on the code of 3dc345a1 and at 53971345, and the mutations of 4d2099b5.
- **r2-green.txt** (round 2): the tip's runs, -race -count=20, the full suites' counts and the pytest target.
- **r3-red.txt** (round 3): this round's tests on the code of 6a6e7ed7, and the mutations of 2486bb00.
- **r3-green.txt** (round 3): the tip's runs, -race -count=20, the full suites' counts and the pytest target.

Paths are written `<clone>` (the lane's clone), `<scratch>` / `<scratch-NAME>` (a git archive, never committed) and `<tmp>`.

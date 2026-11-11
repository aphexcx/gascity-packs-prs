# gp-t542 self-review r1: c1ae854f on 136dbae1

Fresh adversarial review of `git diff 136dbae1 c1ae854f`. The probes live only in scratch clones:
`.gc/gp-t542/review/clone` (detached at c1ae854f; probes `internal/inbound/zz_review_probe_test.go` and `zz_review_probe2_test.go`) and `.gc/gp-t542/review/clone-base` (detached at 136dbae1; probe `zz_review_probe_base_test.go`). No repo file was edited.

Baseline: the five new round-3 tests pass under `-race` at c1ae854f (19.1 s). The related inbound subsets (`Raise|Replay|Mail|Stuck|Partial|Held|Exhaust|Withdraw`), `./internal/server -run 'Replay|Resolve|Status'` and `./internal/ledger` all pass.

## Findings

- **MAJOR: M2 is not fixed in production. Skipping the stamp leaves a mail that gc has, under the per-change dedup key, with no mark. The replayed life's "own" mail of the same kind reuses that key, gc suppresses it and exits 0, and raise stamps it as sent.**
  - Where: `gchat/adapter/internal/inbound/raise.go:65-94` (the `Created.Equal` skip), together with `gchat/adapter/internal/app/operations.go:212-215`. The key there is `gchat:<city>:<identity>:delivery:<sha256(ID)>`, plus `:ended` for an end mail. It is per change ID, not per life. gc's rule is in gascity `cmd/gc/cmd_mail.go:1500-1508`: "the send is suppressed while a previous message with the same key is still live (un-archived)… Suppression exits 0."
  - Why it holds: before this commit, every successful send was stamped, and neither mark is ever cleared (a replay keeps both). So each key went out at most once per change ID, and gc's dedup only absorbed a retry whose result was lost. The skip breaks that invariant.
  - Case A, a waiting mail in flight. Proved by probe `TestProbeReplayAcrossWaitingMailReusesTheDedupKey` in `.gc/gp-t542/review/clone/gchat/adapter/internal/inbound/zz_review_probe_test.go`. The fake raise emulates gc dedup with the app's key and never archives.
    - Sequence: life 1 retries `draft_deferred`. At +2m its waiting mail (key K_w) is in flight. During the flight, POST 6 answers `partial`, so life 1 ends exhausted, and the operator replays. The stamp is skipped (correct so far). A second message then queues behind life 2. At life 2's +2m, its waiting mail goes out under K_w, gc suppresses it, raise gets nil, and `Raised` is stamped on life 2, not before its `Created`. `waitingMailed(life 2)` is true, so `coveredAhead` covers the second message, and that message is never mailed.
    - Output at c1ae854f:
      ```
      head: status intent, created 2026-11-10T20:25:59Z, raised 2026-11-10T20:28:07Z, waitingMailed=true
      head mails delivered by gc: [waiting]; suppressed by gc (exit 0): [waiting]
      message behind it: 0 delivered, 0 suppressed; status intent cause behind_earlier
      HARM: ... the message behind it is covered and never mailed
      --- FAIL: TestProbeReplayAcrossWaitingMailReusesTheDedupKey (0.77s)
      ```
    - The same probe at 136dbae1 (`.gc/gp-t542/review/clone-base/gchat/adapter/internal/inbound/zz_review_probe_base_test.go`) ends in the same state: one mail delivered (life 1's waiting mail), the message behind it never mailed, and life 2 marked as waiting-mailed (`raised 20:26:06Z`). The only difference is that the stamp lands at release instead of 2 minutes later through a suppressed send. In this case the fix changes timing, not outcome.
  - Case B, an end mail in flight (the shape the new test uses). Proved by probe `TestProbeReplayAcrossEndMailBreaksTheWaitingMailsPromise` in the same file.
    - Sequence: life 1 ends `unknown` (status `surprise`). Its end mail (key K_e) is in flight while the operator replays. The stamp is skipped. Life 2 retries, and its waiting mail goes out under a fresh K_w, so it is delivered. That mail promises: "If it ends unresolved for a reason other than this bound, one more mail says so." Life 2 then ends `delivery retries exhausted` (a `partial` at POST 9). Its end mail reuses K_e, which life 1's end mail still holds live, so gc suppresses it. `RaisedEnd` is stamped anyway. The promised mail never arrives, and the agent's latest mail still says the message will be delivered.
    - Output:
      ```
      life 2: status unresolved reason "delivery retries exhausted" attempts 9 raised 2026-11-10T20:26:05Z raised_end 2026-11-10T20:29:05Z
      mails delivered by gc: [ended waiting]; suppressed by gc (exit 0): [ended]
      HARM: life 2's waiting mail said one more mail would say if it ends for another reason; ...
      ```
  - Why the tests miss it: `raiseLog` and `blockedRaise` model no dedup. `TestRaiseCompletingAcrossAReplayStampsNothing` only checks life 2's waiting mail, which uses a fresh key, and never ends life 2.
  - The docs are untrue for the same reason: README.md:349-350 ("mailed on its own terms, as a message never mailed") and CHANGELOG.md:29-30.
  - Fix sketch, preferred because it keeps the spec's "stamp nothing": make the dedup key per life. Carry the life on `Raise` (`Created`, formatted RFC3339Nano) and hash `ID + "\x00" + Created` in `raiseDelivery`, keeping `:ended`. A retry within one life whose result was lost still dedups, and a replayed life gets fresh keys. Add a gc-dedup fake (live keys, nil on suppression) to the round-3 raise tests, and add both probe shapes as tests.
  - Alternative fix: when the record is a later life, stamp `Raised`/`RaisedEnd` with a time before `record.Created`. The race then resolves exactly as if the mail had finished before the replay (the non-race path), and each key stays once per change. This departs from the spec's "stamp nothing", so it is the mayor's call.

- **MINOR: the judgment call (a replay clears `Fragment`) is right, but its cost is undocumented: a replay that meets the leftover fragment retries silently for 24 hours.**
  - Where: `gchat/adapter/internal/server/server.go:1624`, and README.md:378 (the "What to do" cell).
  - Why clearing is right:
    - The replay resets `Attempts` to 0. If the mark were kept, the replay would end after about 7 s of in-gate POSTs whatever gc answered, and a session that is merely busy would bounce the operator's deliberate replay.
    - The bound exists because each `partial` answer can add a fragment. A `failed` answer adds nothing, so life 2's failures are clean retries.
    - A fragment in life 2 marks it again. `TestReplayClearsTheFragmentMark` covers this, and gc's `SummarizeInboundDelivery` ranks partial above failed, so `partial` can never be a duplicate of a whole copy.
  - The cost: if the operator replays without removing life 1's fragment from the agent's input box, gc's draft check keeps answering `draft_deferred`, and life 2 retries for the whole 24 h, holding its space. Because the replay keeps `Raised`/`RaisedEnd` (pre-existing rule), life 2 gets no waiting mail and no end mail.
  - Proved by probe `TestProbeReplayOfAFragmentMessageWaits24HoursSilently` (same file): `life 2: unresolved "not delivered within the retry bound" cause draft_deferred, POSTs in all 1447, mails in all 1`. Keeping the mark would have ended life 2 at four POSTs, also with no mail. The silence is not caused by the clearing; the 24 h hold of the space is.
  - Fix sketch: keep the clearing. Change the README.md:378 cell to: "Look at the session and clear any fragment left in its input box; then `--accept` if the agent has the message, else `--replay` (a replay is not mailed while it waits; watch `gc gchat status`)."

- **MINOR: wording of the four-POST bound is untrue for a late fragment, and "from gc's last answer" mislabels a fragment answer as `not_delivered`.**
  - Where: `raise.go:242`, README.md:303 ("posted at most four times in all"), README.md:378 ("The cause is from gc's last answer"), and `ledger.go:115-118` ("POSTed at most four times").
  - Proved by probe `TestProbeLateFragmentPOSTCount` (`.gc/gp-t542/review/clone/gchat/adapter/internal/inbound/zz_review_probe2_test.go`). Eight `failed` answers then a `partial` at POST 9 give `unresolved "delivery retries exhausted" POSTs=9`, and the end mail reads: `Cause: not_delivered (from gc's last answer; gc answered one of the message's POSTs that a fragment of it is in the terminal, and the adapter stops retrying such a message after four POSTs)`.
    - It was nine POSTs, not four.
    - gc's last answer was `partial`, not "not delivered", because `waitCause(partial)` returns `not_delivered`. This also happens in the classic case of `failed`×3 and then `partial` at POST 4.
  - Fix sketch: say "stops retrying such a message at its fourth POST, or at that answer when it comes later". Name the cause truthfully: when the last answer was itself the fragment, say "gc's last answer was a fragment", or drop "from gc's last answer". Apply the same wording to README.md:303 and `ledger.go:117`.

- **NIT: the test helper `replay` (`round3_test.go:158-172`) re-implements `server.go:1612-1624`.**
  - In `TestReplayClearsTheFragmentMark`, the "replay clears the mark" half is proved by the helper's own `c.Fragment = false`, not by server.go. The server half is covered separately by `status_test.go:124` (`got.Fragment`).
  - Risk: if the two drift, the inbound tests still pass.
  - Fix: a comment that ties the helper to `TestReplayClearsRetryStateAndKeepsRaised`, or one shared replay function.

- **NIT: the log at `raise.go:91-93` reads like a failed mail.**
  - "inbound delivery raise not recorded: the change was replayed or is gone" is logged for a mail that was sent. Suggest: "inbound delivery raise sent, not marked: the change was replayed or is gone".

- **NIT: CHANGELOG.md:42 runs past the paragraph's wrap after `fragment` was inserted.**

## Checked, no finding

- **M1 paths.** These reach `outcome()` and keep the bound:
  - in-gate POSTs;
  - Poll-driven later POSTs;
  - a concluded receipt read by Pass. Probe `TestProbeFragmentViaReceiptKeepsTheBound` passed: `unresolved "delivery retries exhausted" fragment=true POSTs=4`;
  - a Pub/Sub redelivery, which goes through `message()` to `resume`;
  - a restart in the wait (the author's test);
  - a `pending` answer between the fragment and the failures.

  The 0.5.0 crash-window check (`resume`, `Cause == ""`) is unaffected, because every retry write sets `Cause`. `fragment()` is complete for the current gc: partial ranks above failed in `SummarizeInboundDelivery`.
- **Created as a life mark.** It is written only by `record()`, which never overwrites (`message()` resumes an existing ID), and by `resolve --replay`. `.UTC()` strips the monotonic reading, `Clone` copies by value, and the RFC3339Nano round trip is exact. Two lives cannot share a `Created` short of an identical nanosecond. Equality is safe under clock skew. The ordering test in `waitingMailed` is not: a wall-clock step back between recording and mailing reads a same-life waiting mail as an earlier life's. That is pre-existing and outside this delta.
- **Changes/Unresolved copies.** `markUnresolved` writes both from one pointer, the replay deletes the Unresolved copy, and nothing writes one copy without the other, so they cannot disagree in a way the stamp loop would see.
- **New tests.** They are not vacuous: M1 cases assert exactly 4 POSTs or more than 4 and the JSON mark; M2 asserts zero marks after release. They pass under `-race`, and the goroutine that runs a blocked `Poll` touches only the mutexed clock, ledger and fakes.

## Verdict

Not ready. M1 is sound. M2 satisfies the letter of "stamp nothing", but in production gc's per-change dedup key brings back the same harm: proved for a waiting mail in flight, with the same outcome as 136dbae1. It also breaks the waiting mail's promise of an end mail when an end mail was in flight. Fix with a per-life dedup key and a dedup-aware raise fake in the tests.

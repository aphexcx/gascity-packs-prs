# gp-t542 self-review r2: b217a722 (and c1ae854f) on 136dbae1

Fresh adversarial review of `git diff 136dbae1 b217a722`, with most of the attention on `git diff c1ae854f b217a722`, which answers self-review r1.

Method:
- Probes ran only in the scratch clone `.gc/gp-t542/review2/clone`, detached at b217a722. The probe file is `gchat/adapter/internal/inbound/zz_review2_probe_test.go`. No repo file was edited.
- During one mutation run the probe file sat for a moment in `/tmp` and was then moved back into the clone. This was a slip against the scratch-only rule.

Baseline:
- At b217a722 under `-race`, the round-3 tests pass: TestFragmentOnAnyAnswer…, TestFragmentMessageDoesNotHoldItsSpace, TestReplayClearsTheFragmentMark, the three TestRaiseCompletingAcross…, and TestDedupKeyIsPerLifeAndKind (`ok … 19.939s`).
- `go test ./internal/app -run 'TestDeliveryRaise|TestEndedDeliveryMail'` passes.
- The inbound subset `TestEndMailIsNotShadowed|Raise|Mail|Replay|Withdraw` passes, apart from my own probe.
- `gofmt -l` prints nothing, and `go vet ./internal/inbound ./internal/app` is clean.

## Findings

### MINOR 1: the docs still promise "at most twice" per message, "its end is mailed once" and "as a message never mailed". The fix itself breaks all three, and its own test asserts 3 mails

**Where.** These sentences are untrue at b217a722:
- `gchat/README.md:343` ("A message is mailed at most twice")
- `gchat/README.md:350` ("its end is mailed once")
- `gchat/README.md:351-352` ("the replayed message is then mailed on its own terms, as a message never mailed")
- `gchat/CHANGELOG.md:27` ("no message is mailed more than twice"), in the same bullet as the new sentence at :28-30
- `gchat/adapter/internal/ledger/ledger.go:125` ("A change is mailed at most twice")
- `gchat/adapter/internal/inbound/raise.go:99` ("A message is mailed at most twice")

**Case 1: a waiting mail in flight across a replay.** This is the delta's own test, `round3_test.go:302`. It requires `len(got) == 3` mails at gc for one message: life 1's end mail, then life 2's waiting mail, then life 2's end mail.

**Case 2: a stamped waiting mail, then the end mail in flight across a replay.**
- Sequence:
  1. Life 1 retries `draft_deferred`. Its waiting mail goes out at +2m and is stamped.
  2. POST 6 answers `surprise`, so life 1 ends `unknown`, and its end mail is in flight.
  3. The operator replays, and the stamp is skipped.
  4. Life 2 keeps `Raised` from life 1, so it gets no waiting mail. It is not "a message never mailed".
  5. Life 2 then ends `surprise` and gets an end mail.
- At gc that is 3 mails, two of them end mails.
- PROVED by probe `TestProbe2ReplayAcrossEndMailAfterAStampedWaitingMail` in `.gc/gp-t542/review2/clone/gchat/adapter/internal/inbound/zz_review2_probe_test.go`:
  ```
  life 2 after the in-flight end mail: intent created 2026-11-10T20:26:14Z raised 2026-11-10T20:24:58Z raised_end 0001-01-01 00:00:00 +0000 UTC waitingMailed=false
  after 10 minutes of life 2: 2 mails for the message at gc (kinds [waiting@20:22:58 ended@20:22:58]); 1 for the message behind it
  after life 2 ends: 3 mails for the message at gc (kinds [waiting@20:22:58 ended@20:22:58 ended@20:26:14])
  WORDING: one message got 3 mails; README says 'A message is mailed at most twice', CHANGELOG 'no message is mailed more than twice'
  WORDING: life 2 got no waiting mail of its own; README says the replayed message is then mailed 'as a message never mailed'
  ```

**The behaviour itself is within the binding rule** (at most twice per life; a replay keeps the marks of mails that completed). Only the text is wrong.

**Fix sketch.**
- Say "at most twice in each life of a message (a replay starts a new life)" in all six places.
- Replace "as a message never mailed" with "as if that mail had not been sent: the marks of mails that completed before the replay still hold".
- Replace "its end is mailed once" with "once per life at most".

### MINOR 2: no test pins the rule that a lost-result mail stays one mail at gc within a life through `report()`. A key that changes on every send passes the whole inbound package

**Where.** `gchat/adapter/internal/inbound/raise.go:300` (`Recorded: c.Created`) and `inbound.go:122-129` (`DedupKey`).

**The gap.**
- `TestDedupKeyIsPerLifeAndKind` and the app tests build `Raise` by hand with a fixed `Recorded`.
- Every inbound test clock runs on whole seconds (`retryStart` is `…:58Z`, `RetryDelay` is 1 s).
- No test retries a same-kind mail after a lost result. `TestEndMailIsNotShadowedByALostWaitingMail` retries as an end mail, which has a different key anyway.
- No test carries a sub-second `Created` through a restart.

**PROVED by mutation, in the clone.**
- With `raise.go:300` changed to `Recorded: now` (a key per send), one full `go test ./internal/inbound -count=1` gave `ok gchat-adapter/internal/inbound 35.185s`. Nothing failed.
- Probe `TestProbe2LostMailAcrossRestartKeepsOneMail` (same file) does catch it, with `HARM: a lost-result mail retried after a restart is 2 mails at gc`. The probe's sequence:
  1. A real `time.Now()` clock with nanoseconds and a monotonic reading.
  2. The waiting mail lands at gc, but its result is lost.
  3. A restart through `ledger.Load`.
  4. A retry.

**At b217a722 the code is right.** The same probe passes:
```
created in memory: 2026-11-11T02:34:49.053702Z (ns 53702000)
created after reload: 2026-11-11T02:34:49.053702Z equal=true
key before delivery:b8fa84a4…766b; retry keys [delivery:b8fa84a4…766b]; delivered 1; raised 2026-11-11 02:39:19.053702 +0000 UTC
```
Statically, too, every path holds the key within a life:
- `Created` is written only by `record()` (`p.opts.Now().UTC()`) and by `resolve --replay` (`server.go:1618`, `time.Now().UTC()`). Nothing deletes `l.Changes` entries, so `record()` never rewrites a live change.
- `.UTC()` strips the monotonic reading.
- The JSON round trip is RFC3339Nano and exact.
- `DedupKey` normalises the zone with `.UTC()` before formatting.
- `markUnresolved` writes the Changes and Unresolved copies from one pointer.

The only `inbound.Raise` constructions are `raise.go:300` in production, `raise_test.go:39/64/91` and `round3_test.go:392-398`. The one at `raise_test.go:64` has no `Recorded`, but it is the Notify-hook path, where the key is unused.

**Why it matters.** The rule must not move, yet nothing guards it at the level where it can break. A zero `Recorded` (a future `Raise` built without it) also falls back silently to a per-change key, which brings r1's MAJOR back with no failing test.

**Fix sketch.**
- Add the probe as a test: lost result, `rf.restart`, retry gives one mail at gc, using a sub-second clock.
- Add a check that every `report()` Raise carries `Recorded == c.Created`.
- Optionally, have `DedupKey` refuse a zero `Recorded`, or have the processor log and skip that send.

### NIT 3: a replayed life's end mail, reached only through the new race path, opens as the follow-up of a waiting mail, although the agent's latest mail already said the message ended

**Where.** `raise.go:201` (`second := !waiting && !c.Raised.IsZero()`) and `raise.go:213-214`.

**What happens.**
- In Case 2 of MINOR 1, the third mail opens: "An earlier mail said this Google Chat message was waiting and would still be delivered. It has ended unresolved…". This is PROVED in the same probe output (`last mail opens: …`).
- The agent's previous mail was life 1's end mail ("stopped trying … waits for an operator"). Nothing in the new mail says that a replay happened in between.
- Before b217a722 this mail was never sent:
  - at 136dbae1 the stamp landed on life 2;
  - at c1ae854f gc dropped the send under the shared key.

**Fix sketch.** When `c.Raised.Before(c.Created)` (a mark from an earlier life), open with something like: "An earlier mail named this message. It was replayed (`gc gchat resolve --replay`), and the replay has ended unresolved…".

### NIT 4: two places still say "four-POST bound" for the fragment rule after r1 MINOR-2 was fixed

**Where.**
- `gchat/CHANGELOG.md:9-12`: "`partial` keeps its four-POST bound … also when gc answers the message's other POSTs `failed`".
- `inbound.go:1169-1175`, the `outcome` doc comment.
- The `round3_test.go:28` comment.

**What is true.** A fragment first met after the fourth POST ends the message at that POST, for example POST 9 (r1 probe `TestProbeLateFragmentPOSTCount`). README:302-305, `raise.go:242` and `ledger.go:115-119` now say this; these places do not. Traced statically.

**Fix.** Use the README wording: "is not posted after its fourth POST, or after that answer when it comes later".

### NIT 5: the exhausted mail's Cause line no longer explains the cause word

**Where.** `raise.go:241-242`.

**What happens.** The cause is the last answer's `waitCause`, via `unresolvedCause` at `inbound.go:442-452`.
- With the answers partial, failed (draft), failed, failed, the line reads "Cause: draft_deferred (gc answered one of the message's POSTs that a fragment of it is in the terminal; …)".
- Every other cause line puts the meaning of its word in the parenthetical. Here the parenthetical is not the meaning of `draft_deferred`, and the operator loses "text in the agent's input box held the message back". That is the very thing the new README cell tells them to clear.
- A `held` answer also maps to `draft_deferred` (`waitCause`, `inbound.go:426-427`).

Traced statically.

**Fix sketch.** `line("Cause", c.Cause+" ("+causeMeaning[c.Cause]+"; gc answered one of the message's POSTs that a fragment … later)")`.

## Checked, no finding

- **The per-life key in production.**
  - The app sends `gchat:<city>:<identity>:` + `r.DedupKey()` (`operations.go:214`). That is the same shape and length as before, with the change ID hashed and never in clear. The new log line (`raise.go:92`) carries only the change ID, as the existing lines do, and no customer text.
  - No other user of the old key format exists. Status, e2e, startup and the notifier do not use it. Only historical evidence files (`assets/pr-evidence/gp-dl26/sample-mail-argv.txt`, `gp-bnuq`) show the old key, and no generator regenerates them.
  - The mail feature is unreleased (CHANGELOG `## Unreleased`; 0.5.0 has no raise mail), so no upgrade window exists in which a lost-result mail under the old key is resent under the new one.
- **The new tests are not vacuous.**
  - With `DedupKey` mutated back to `sha256(r.ID)`, three tests fail with exactly the commit's numbers:
    - `TestRaiseCompletingAcrossAReplayStampsNothing`: "raised_end … 2 mails at gc";
    - `TestRaiseCompletingAcrossAReplayOfAWaitingMessage`: "waiting mails at gc for the two lives: 1";
    - `TestDedupKeyIsPerLifeAndKind`.
  - Reverting the `Created.Equal` skip fails both replay tests at their "raised … raised_end" checks (traced: the record would carry the marks).
  - The gated tests run the blocked `Poll` only in its raise phase, which is outside the gate. Everything they touch is under `LedgerMu`, `gcMail.mu`, the clock mutex or fakegc's lock. `-race` is clean.
  - Nothing was weakened from c1ae854f: the old `blockedRaise` assertions moved onto the dedup-aware `gcMail`.
- **M2 sequences under the per-life key.**
  - A waiting or end mail in flight across a replay gives life 2 fresh keys.
  - A replay and life 2's end, both inside the flight, mark nothing.
  - A mail in flight while the same life ends or is accepted is marked.
  - A lost-result mail followed by a replay gives one mail per life.
  - A ledger-save failure after the send, retried in the same life, keeps the same key, and gc absorbs it.
- **M1 (c1ae854f).** I found nothing r1 missed:
  - the fragment mark reaches `outcome()` from the in-gate POSTs, from the later POSTs Poll drives, and from a concluded receipt;
  - `pending` with a fragment member concludes through `concludedPending`, not a retry;
  - `resume` still ends a 0.5.0 crash-window record;
  - a replay clears the mark (`server.go:1624`; `status_test.go` checks it).
- **The README table cell's claim that a replay "meets `draft_deferred`".** gc's composer check at gascity 895b2094d, `internal/runtime/tmux/tmux.go:2893-2937`, defers any nonempty draft that `nudgeDraftMatches` does not match against the whole message. A fragment does not match, so the cell's statement holds. "Usually with no mail of its own" is right because life 1's end mail stamped both marks.

## Status of the r1 findings

- **r1 MAJOR (per-change key defeats the skip): fixed.**
  - `DedupKey` = `sha256(ID + "\x00" + Created RFC3339Nano)`, plus `:ended` for an end mail.
  - Both r1 shapes are now tests that fail under the old key.
  - The key is stable across a restart (probe R2-1).
  - What remains is the docs (MINOR 1) and a missing stability test (MINOR 2).
- **r1 MINOR-1 (the cost of a replay into a box that still holds the fragment was undocumented): fixed.** The README:382 cell says to clear the fragment first and describes the silent 24 h wait. Its wording checks out against gc's code.
- **r1 MINOR-2 (four-POST wording; "from gc's last answer"): fixed, with two gaps.**
  - Fixed in README:302-305, the README table, `raise.go:242` and `ledger.go:115-119`.
  - Left in CHANGELOG:9-12 and the `outcome` comment (NIT 4).
  - The cause line lost the meaning of its word (NIT 5).
- **r1 NIT (the `replay` helper mirrors server.go): left on purpose.** The commit says so, the helper's comment names server.go, and the server half is `status_test.go:124`. Acceptable.
- **r1 NIT (log wording): fixed** at `raise.go:92`.
- **r1 NIT (CHANGELOG wrap): fixed.**

## Verdict

No BLOCKER or MAJOR. M1 and M2 hold in production with gc's real dedup, and the per-life key is stable within a life across a restart. It is ready once the wording in MINOR 1 is folded in and the stability test from MINOR 2 is added.

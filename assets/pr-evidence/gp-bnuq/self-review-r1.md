# gp-bnuq self-review r1: fresh adversarial read of 65e502f4 (base d2f453b8)

Reviewer: fresh Opus subagent, no part in writing the change. Read: the whole spec
(gchat-adapter-deferred-delivery-retry-and-raise-20261110.md), the whole diff, every touched
function in context (inbound.go whole, raise.go, ledger.go, server.go status/resolve, app
operations/workers, gcclient, wire, fakegc), the catch-up runner where it calls the processor,
and gc at a8217556 / ee972774 (inbound_delivery, inbound_member, ClaimFor, the nudge paths).

Baseline: `go test` of inbound, ledger, server, app, wire, gcclient and fakes is green; `go test -race
./internal/inbound` green; `gofmt -l .` empty; `go vet` clean; the two new Python status tests green
(unittest). Probes ran in the scratch clone .gc/gp-bnuq/review-scratch only; it is back at HEAD
with the probe files removed. The worktree was not touched.

Counts: BLOCKER 0, MAJOR 2, MINOR 7, NIT 11.

No duplicate-POST path was found. Every route back to intent is a definite "not delivered"
(failed/held, partial below its bound, or the unchanged 5xx/dial/stopped-city rule). The attempt
is still saved before every POST. "partial" at four or more POSTs ends unresolved in a single
save. gc PR 99 does not queue a held inbound message for its own later delivery (no
nudgequeue use on the extmsg path), so retrying after "held" is clean. No space can stay
blocked forever: the head of a space always moves forward, ends at its bound, or is a pending
change whose hold is bounded. The raise flag is written to both the Changes and Unresolved
records, and markUnresolved carries it across. resolve copies the flag from Unresolved.
Nothing in the mail, the logs or the ledger carries message text, the sender or gc's error
text.

---

## MAJOR

### M1. Ordering plus the 2-minute raise turns normal back-pressure into "stuck" mail and serializes a burst (spec-conformant; needs the mayor's ruling)

Where: inbound.go:376-386 (heldBack, bounded pending block), raise.go:41-45 (intent or pending
at least StuckAfter since Created is due).

Scenario (VERIFIED by a probe in the scratch clone): five messages arrive in one space within
5 s while the agent is busy. gc answers every POST "pending"; this is the spec's own reading
of a busy session ("A busy session therefore gets 'pending'"). Each receipt concludes "delivered"
35 s after its POST, and Poll runs every 5 s. Result:

```
b1 posted=+0s    b2 posted=+35s   b3 posted=+1m10s   b4 posted=+1m45s raised   b5 posted=+2m20s raised
2 raise mails for a burst that flowed normally
```

0.5.0 would have POSTed all five at once, and each would have concluded about 35 s later with no
backlog. Now each later message waits for the earlier receipt to conclude. The fourth and later
messages pass StuckAfter from their own recorded time and are mailed as stuck
("behind_earlier", or "waiting for gc's receipt") while nothing is wrong. The same happens in
the draft-deferral case the bead exists for. One stuck head mails once, and then every message
queued behind it in its space is mailed too, four per pass. A 20-message backlog means 20+
mails to the mayor in about 25 s.

This is not a departure from the text. (3) and (4) together say exactly this. The spec does
not weigh the consequence, though, and the mayor ruled on the pending block only as "STAYS
WITH A BOUND".

Suggested fix, for the mayor to rule: do not raise a change whose cause is behind_earlier
(raise the head of its space, which carries the real cause, and name in that mail how many
messages wait behind it). Or, for a held-back change, count StuckAfter from the moment its
space's head became stuck. That keeps one mail per stuck space and makes a busy-agent burst
silent. The serialization (slower delivery of bursts to a busy agent) is the price of the
[ADDED] pending block, and the ruling should name it.

### M2. A retrying or held-back change keeps catch-up open for up to 24 h; catch-up then rewrites the whole ledger (fsync) once per non-terminal item every second, and health reads "catching-up"

Where: interaction of this change with catchup.go:536-553 and app/workers.go:249-269. Both
files are out of scope, which is why this needs a scope ruling.

Mechanism:
- Catch-up keeps a backlog item while a related change is neither committed nor unresolved
  (catchup.go:538).
- For each such handled item it does an r.update that saves the ledger every pass, with no
  errUnchanged (catchup.go:551).
- remaining > 0 keeps `caught` false, so the app re-runs Pass every 1 s (workers.go:251,
  261-269), and the monitor never moves health to "ready".

Before this commit a "failed" change ended within about 7 s, and only pending receipts held an
item (up to 1 h). Now a failed or held change stays intent for up to RetryBound (24 h). Every
change queued behind it is intent (behind_earlier) for as long as its head is stuck. The hourly
history sweep puts any recently received message back in the backlog, so this triggers whenever
a retry episode overlaps a sweep.

VERIFIED (catch-up package probes, scratch clone):
- One recovered message answered "failed" each time: every Pass reports remaining=1 and makes 1
  ledger save, pass after pass; the change stays intent with 4 attempts and cause draft_deferred.
- Ten recovered messages of one space, the first answered "pending":
  - this commit: 1 POST, remaining=10, 10 ledger saves per Pass;
  - base d2f453b8: 10 POSTs, 9 delivered, remaining=1, 1 save per Pass.

The 1-second cadence comes from reading workers.go; it was not measured. At that cadence, a
replay or burst into a space whose head is draft-deferred costs N fsync'd full-ledger writes
per second, for hours. The ledger is never pruned and carries every turn's Event JSON.
Meanwhile `gc gchat status` shows "Health: catching-up, recovering retained history" and a
nonzero Backlog.

Suggested fix (needs the ruling, since it touches catch-up): count a change in intent with
reason "retry", or with cause behind_earlier, as settled for catch-up's purposes. The ledger
already owns it and Poll drives it. The smaller alternative is for drain's non-terminal
update to return errUnchanged when the item it re-encodes is unchanged. Either way, add a
catch-up test with a retrying change.

---

## MINOR

### m1. A pending row keeps a stale next_attempt; status prints a past "next attempt" for it

Where: inbound.go:1035-1041 (the attempt update does not clear NextAttempt), inbound.go:1172
(the "pending" outcome clears Cause but not NextAttempt), status.sh:30 (`waiting` includes
pending), server.go row building.

Scenario (VERIFIED by a probe): four "failed" answers, then a fifth POST answered "pending". The
change is pending with attempts=5 and next_attempt=20:23:13Z, which is the moment that POST
was made. `gc gchat status` prints "pending …; next attempt 2026-11-10T20:23:13Z; posts 5" and
keeps printing it for up to an hour. The JSON row carries next_attempt too.

Fix: clear NextAttempt in the attempt update (it has been consumed by then), or in the
"pending", "delivered" and "no_route" outcomes. Add a status assertion that a pending row
has no next_attempt.

### m2. The resolve --replay HTTP handler now runs the raise phase synchronously; a slow mail can make the CLI report failure after the replay was saved. The app's poll loop also stalls for the length of the raise phase

Where: server.go:1636 calls `poll(r.Context())`, which is proc.Poll, which is pass followed by
raise (inbound.go:239-243). raise makes up to 4 calls at 10 s each (operations.go:204, 216).
The CLI's curl uses --max-time 45 (commands/common.sh:19).

Scenario (reasoned): an operator runs `gc gchat resolve ID --replay` while 4 changes are due
for a raise and `gc mail send` hangs. The handler's pass runs, then up to 40 s of mail. curl
times out, and the CLI prints an error although the replay was saved. A second try then gets
400 "unresolved delivery not found". The same 40 s blocks the app's poll goroutine
(workers.go:322-347), so retries and receipt polls wait. The gate is not held, so this is
latency, not a deadlock.

Fix: give server.SetPoll a pass-only function (export Pass, or add a `Recover` that skips the
raise), or run the raise in its own goroutine owned by the app loop. Bound the whole raise
phase, not just each call.

### m3. The waiting mail promises the bound is the only way it ends; other unresolved endings that need an operator send no mail

Where: raise.go:164 ("This is the only mail for this message. If it has not landed by STOP, the
adapter stops trying and gc gchat status lists it for gc gchat resolve.").

Scenario (VERIFIED by a probe): the change is raised while waiting (draft_deferred). A later
retry POST loses its response, so the change becomes unresolved "POST response unavailable;
delivery unknown", cause unknown. No further mail is sent, which is per the spec ("A change
raised while waiting is not raised again"). The only mail told the agent the adapter keeps
trying until the next day. The same silence follows "held" (an operator is needed),
"receipt pending beyond deadline" and a rejected POST.

Fix, in scope: reword the waiting mail so it does not promise one ending. For example: "If it
ends unresolved, for any reason, gc gchat status lists it for gc gchat resolve; no further
mail is sent." Spec question for the mayor: should an operator-needed ending (held, unknown)
raise again despite an earlier waiting raise?

### m4. The mail's read command often cannot show the message

Where: raise.go:160 (`--since Created - 1h`). The server's read filters `create_time > since`
(chat.go:197).

Scenario (reasoned): Created is when the adapter recorded the change, not the Chat create
time. The read misses the message for:
- catch-up and first-start replays of older history;
- an edit (`#updated#…`) or deletion of a message created more than an hour before;
- a reaction on an older parent (c.Message is the parent);
- every operator --replay (resolve resets Created to now).

The mail tells the agent "Read the message … with the command above".

Fix: bound --since by the message's own time. MessageState.Revision is the create time for a
created message; keep an hour of margin. Or, when a thread is known, drop --since and rely on
--thread with a --limit.

### m5. A "partial"-exhausted change is mailed with a false cause meaning

Where: inbound.go:1174 (partial gets `waitCause`, usually not_delivered), raise.go:100
(not_delivered means "gc answered that nothing was delivered").

Scenario (reasoned): a change answered "partial" four times ends as "delivery retries
exhausted", cause not_delivered. The mail says "Cause: not_delivered (gc answered that nothing
was delivered)", but a fragment is in the terminal. That fact matters for the accept-or-replay
choice the mail then offers. The README table gets this right; the mail does not.

Fix: when Reason is reasonExhausted, print a meaning that says a fragment may be in the
terminal. The closed set stays as it is.

### m6. RetryBound now ends changes that only wait because gc is down; the spec contradicts itself here, and the cause word is wrong for that case

Where: inbound.go:1003-1005 (the bound comes before the POST), inbound.go:1054-1063 (the
5xx/dial/stopped-city path), inbound.go:426-430 (a bound with no cause becomes not_delivered).

Scenario (reasoned): gc's API is down or stopped for more than 24 h while the adapter runs.
0.5.0 retried those changes forever, since Attempts-- never reaches 4. Now each becomes
unresolved "not delivered within the retry bound" at Created+24h and must be replayed by hand.
Its cause reads not_delivered, "gc answered that nothing was delivered", although gc never
answered. Spec (2) says both "the rule for a gc that is down or restarting does not change"
and "the bound applies to every change in intent".

Fix: get a ruling. If the bound stays, give a change that never got a definite answer a
truthful cause (unknown, or a new word in the closed set).

### m7. Spec-level: "landed_unconfirmed" can commit, silently and with no raise, a row gc never counted

Where: inbound.go:467-474 (landedWhole), following spec (6) exactly.

Reasoned from gc a8217556 internal/extmsg/inbound_member.go: one row is "landed, runtime did not
vouch (Submit == ""), send path errored". gc reports that row as pending with DeliveredBytes ==
ExpectedBytes, the reminder's own length, not a count. Another row is "landed whole, then the
send path errored". There the paste sits unsubmitted in the composer. Both pass the adapter's
"every member pending with delivered == expected > 0" test and commit with no raise. On the tmux
runtime the runtime does vouch, so the first case is rare. It is still the one silent drop
left.

Not a departure from the spec. Flag it to the mayor. A possible refinement is to require
some positive evidence beyond bytes, but the wire carries none except the error text, which
the spec rightly forbids storing.

---

## NIT

- n1. raise.go:26 and the loop. The mails go out from one snapshot over up to 40 s. A change that
  committed in the meantime still gets "has not reached the agent yet" and is then marked
  raised. Re-read the change under the lock just before each call and skip it if it is no
  longer due.
- n2. inbound.go:118-120. raiseTried lives in the Processor. A generation rebuild (reconnect or
  credential reload) forgets it, so a failing send is retried at once instead of after a
  minute. The once-a-minute rule also measures from the start of the raise phase
  (raise.go:67, 82), not from the failure.
- n3. status.sh:31 prints "posts N" only when N > 0. Spec (5) says the count prints for a
  waiting row. An empty reason renders as "intent id: ; age …".
- n4. inbound.go:1157-1168. A committed record keeps its last Cause, and NextAttempt
  (see m1). The ledger field comment says cause is for "waiting or unresolved".
- n5. Clock rewinds are guarded only for NextAttempt (inbound.go:1020). With the wall clock
  set back an hour:
  - the pending block (inbound.go:381, Posted + StuckAfter) holds a space for about 1 h;
  - the raise (raise.go:43, Created + StuckAfter) stays quiet for about 1 h.
- n6. inbound.go:412. The match on "pending interaction" also catches gc's "session has no
  pending interaction" and "getting pending interaction: …" errors. The word only feeds status
  and mail.
- n7. raise.go:90-95. `hours(time.Hour)` prints "1 hours".
- n8. A pending change carries no cause word, so the mail prints "Cause: none recorded" and
  the status row has no cause. Spec (4) says a waiting change has one of four words, and none
  of them fits "pending at gc".
- n9. catchup.go:651. Catch-up's "malformed catch-up history" unresolved rows are created
  without a cause, so they are never raised. Spec (4) says "whatever the reason". Catch-up is
  out of scope, so this needs a note, not a change here.
- n10. fakegc. On a redelivery after a receipt concluded "pending", the fake answers the
  original POST response with only its status updated. Real gc (extmsgClaimInboundFanout)
  answers the concluded delivery itself: members, terminal flag, original receipt id. The
  adapter's result is the same (pending, then poll, then judge), so this matters only if a
  future test relies on members in that answer.
- n11. A 0.5.0 crash-window record is reposted by this binary. Such a record is intent/retry
  with 4 attempts, saved between 0.5.0's "partial" save and its unresolved save. This binary
  sees later phase and NextAttempt zero, so it POSTs at once, possibly adding a fragment.
  0.5.0 stored no cause, so it cannot tell. The window is extremely narrow. Also, spec (l)
  asked for the dedup key and agent to be proven "through the existing test hook". The hook
  cannot carry them, and the test uses a GC_BIN fake instead, which is fine.

---

## Checked and found sound (no finding)

- Spec (2): the first four POSTs are unchanged; the waits after are 8, 16, 32, 60, 60 s; the cap
  is applied before overflow (retryWait); at most one POST per call in the later phase; none
  before next_attempt; a Pub/Sub redelivery takes the same path; no later-phase wait is spent
  in the gate; partial is bounded in one save; held is retried from the first POST; an unknown
  status is unresolved with cause unknown.
- Spec (3): the order key is Created then ID, the same as Poll's sort; the pending block is
  bounded from Posted; committed and unresolved block nothing; other spaces flow; a space
  drains in one pass (the snapshot is re-read per resume).
- Spec (4): the raise runs outside the gate and the ledger lock; TryLock keeps two Poll
  callers apart; at most four per pass; raised is written only after nil, on both maps; one
  minute throttle; withdrawn and cause-less old rows are skipped; the key is
  gchat:CITY:IDENTITY:delivery:hex(sha256(id)); the subject; the 10 s limit; the body leaks
  nothing private (test-checked).
- Spec (5): JSON keys are omitted when empty, stuck is added, and an old answer prints exactly
  as before (Python unittest green).
- Spec (6): the three judgements are in the one place; the counters; the Info line.
- Spec (7) and (8): replay clears next_attempt, posted and cause and keeps raised; retry state
  survives a restart; an in-flight POST ends interrupted.
- Ledger: 0.5.0 bytes round-trip; new fields are omitzero/omitempty; 0.5.0 ignores unknown keys
  on read (and drops them on save, as the DELIVERY NOTE says).
- gc semantics: failed and held mean nothing landed; ClaimFor re-claims after failed, held and
  partial and holds on pending and delivered; the fake matches except n10.
- Privacy: no customer, person or real id in the commit, tests, fixtures or docs.

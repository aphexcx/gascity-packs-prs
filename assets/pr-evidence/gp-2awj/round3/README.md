# PR 104 round 3: bring current with main 205d0aec

Bead gp-lt65. Round 2's head is `824ebab97dc6b048ddca4edb06b43af2d902495d`. The new base is
main at `205d0aecedf944792f1e3b3fbc420b9e5ad910d8` (PRs 103, 95, 90, 98 and 102 since round
2's base `c99694367081b2b3e98a58aafaf1320e091621fe`). This round's commits, on the same branch
gp-2awj:

| commit | what |
| --- | --- |
| d17a0c23 | the merge of main 205d0aec (a merge commit; parents 824ebab9 and 205d0aec) |
| 87778c00 | item 4: one test per adapter with both switches on, red on Slack's company routes |
| 16815df7 | the Slack fix for R5 |
| 20b8c2c0 | item 3: the off-parity tests re-pinned to main 205d0aec, with a NEVER_SILENT=on run each |
| 2db97763 | self-review MINOR 1 (part) and NIT 2: the acknowledgement's own echo fed back in the item 4 tests; a parity comment's line; the code head |

## The merge (R1, R7)

`git merge-tree --write-tree 824ebab9 205d0aec` and the merge itself agree: nine files conflict,
thirteen hunks in git's default conflict style, every one against 863e9aa8 (PR 98, the
never-silent notices). The branch merges clean with main up to 810a6179. Round 2's READY said
the tip conflicts in four gchat files; the merge shows nine files in three adapters. (In diff3
style, outbound.js's three hunks show as five regions.)

Every hunk keeps both sides. A line of either side that the resolution drops or changes is
named in the last column.

| # | file (line in d17a0c23) | main had | the branch had | kept | dropped or changed, and why |
| --- | --- | --- | --- | --- | --- |
| 1 | gchat/adapter/internal/app/workers.go:85 | `inbound.Options{NeverSilent: a.neverSilent, ...}` | `inbound.Options{Ack: a.arrival, ...}` | one literal with both fields | none; two independent options of the same call |
| 2 | gchat/adapter/internal/chat/chat.go:228-231 | `CreateMessageWithAttachmentsAndTime` returns `createdAt` parsed from Google's create time (four results) | the `MessageCreated` hook, then `return result.Name, threadName, nil` (three results) | the hook, then main's `createdAt` and four-value return | the branch's three-value return: main moved the body into `...AndTime` and made `CreateMessageWithAttachments` its wrapper. The hook now sits in the one function every mayor post reaches (the server's publish calls `...AndTime`). Main's `CreateNotice` (chat.go:157) stays outside it |
| 3 | gchat/adapter/internal/config/config.go:109-117 | `NeverSilent struct{...}` field | `Acknowledgement ack.Options` field | both fields | none (gofmt realigned the block) |
| 4 | gchat/adapter/internal/inbound/inbound.go:38-40 | `NeverSilent *neversilent.Engine` | `Ack *ack.Service` | both fields | none |
| 5 | gchat/adapter/internal/server/server.go:1367-1380 | after a placed reaction with a `session_id`: `notices().Act(space, "", session, now)` | after a placed reaction: a stopwatch `reaction` line, actor agent | both, the stopwatch line first | none; each records the mayor's own react verb for its own feature |
| 6 | gchat/adapter/internal/server/server.go:1706-1712 | `status["never_silent"]` | `status["jev_api_key"]` "set" or "not set" | both fields | none |
| 7 | slack-full/adapter/main.go:404-409 | `neverSilent *neversilent.Engine` | the five arrival fields | all six | none |
| 8 | slack-full/adapter/main.go:2992-2995 | `cfg.recordNeverSilentWords(...)` for a delivered upload | `go recordUploadedFile(...)` for the stopwatch | both | none |
| 9 | whatsapp/adapter/src/inbound.js:137-138 | `notices` parameter | `arrival` parameter | both | none |
| 10 | whatsapp/adapter/src/index.js:38-40 | `import { createNeverSilent }` | `import { createChoice, createArrival }`, `import { createStopwatch }` | all three | none |
| 11 | whatsapp/adapter/src/outbound.js:519-525 (sendChunks) | `({ answer, activity } = await sendWithActivity({ ..., idempotency_key: textChunkKey(...) }, conversationId, session))` | `sendKey`; `await stopwatch?.attempt(sendKey)`; `gateway.send({ ..., idempotency_key: sendKey })`; the no-message-id throw; `stopwatch?.words(...)` | the marker before, main's `sendWithActivity` as the send, the words line after | the branch's direct `gateway.send` and its no-message-id throw: `sendWithActivity` makes the same send and throws the same error (outbound.js:287-303). Main's inline `textChunkKey(...)` becomes the branch's `sendKey`, the same value |
| 12 | whatsapp/adapter/src/outbound.js:648-653 (the file send) | `request`; `sendWithActivity` only when there is a caption, else `gateway.send(request)` | `await stopwatch?.attempt('file-send:...')`; an inline `gateway.send({...})` | the marker, then main's `request` and its two sends | the branch's inline send: main's `request` carries the same fields. The words line after the send (outbound.js:656) merged clean |
| 13 | whatsapp/adapter/src/outbound.js:750-755 (sendUpdate) | `({ answer, activity } = await sendWithActivity({ ..., idempotency_key: templateKey(...) }, to, session))` | `sendKey`; the marker; `gateway.send`; the no-message-id throw; `stopwatch?.words(...)` | the marker, `sendWithActivity`, the words line | as row 11 |

One change the text merge took without a conflict and that this PR had touched: Slack's
`recordDeliveredPublish` (main.go). Round 2 restored the base's `replyTo != ""` condition there
(case 1f, "a root publish journaled as a reply"). Main's 863e9aa8 removed that condition itself,
so a root publish is journaled as a reply on main too, and the merge follows main. The Slack
off-parity expectation changed with it (item 3 below).

## R6: what never-silent and the stopwatch each read, per adapter

Lines are at 20b8c2c0, unchanged at 2db97763. R4 as the bead states it (the acknowledgement is
never taken for the session's answer) holds on every route. The reverse case, a session
reaction that the acknowledgement hides from never-silent, is the self-review's MAJOR 1 below,
open for a ruling.

**Google Chat: R4 and R5 hold as merged; no code.** Never-silent learns that the session
answered only from `Act`: the publish handler after each part Google accepted, with the
request's session (internal/server/server.go:673); the react verb when it carries a
`session_id` (server.go:1373); the ledger's sends with a session, reconciled at start
(internal/inbound/never_silent.go:23). The acknowledgement goes out through
`chat.CreateArrivalReaction` (internal/arrival/arrival.go:63, internal/chat/chat.go:394), which
calls no `Act`; Google's echo of it is a reaction event, and the inbound path calls no `Act` for
reaction events. The stopwatch learns words from the `MessageCreated` hook (arrival.go:73),
which runs inside `CreateMessageWithAttachmentsAndTime` (chat.go:229), the call every mayor post
reaches. The never-silent line goes through `CreateNotice` (server.go:781, chat.go:157), which
has no hook. Google's echo of the line is a message from the identity, and the acknowledgement
skips the identity's own messages (internal/inbound/inbound.go:219).

**Slack: R4 holds as merged; R5 needed code (16815df7).** Never-silent learns that the session
answered from: the react verb, a reaction placed with a session (main.go:3332); a delivered
publish (main.go:3356, `recordDeliveredPublish`); the words of a post or an upload
(never_silent.go:107, from main.go:2799 and main.go:2995); and at each tick, reconciled, the
publish receipts (never_silent_recovery.go:151), the journal's session acts whose source is
`session_publish` or `session_react` (never_silent_recovery.go:197-198), and the company reply
commands' intents and activity files (never_silent_recovery.go:241, :277). The acknowledgement
is a `reactions.add` from `slackArrivalSender` (arrival.go:74): no session act, no journal act,
no intent. The stopwatch learns words from a session act of kind reply
(ingestion_session_act.go:48), a publish Slack accepted (stopwatch_publish.go:13), an upload's
shares (stopwatch_file.go:33) and, on company routes, a confirmed agent post seen at ingress
(stopwatch_company.go:25, reached from company_delivery.go:847, company_dm.go:141 and
company_mpim.go:188). The never-silent line is posted by the engine's `Post` with
`postToSlack` (never_silent.go:57), outside the publish handler. On the switchboard its echo is
the bot's own message, which no words source reads. On a company route the line is posted with
the agent's own bot token, so its echo reached `recordCompanyWords` as that agent's post and
was timed as words. The fix skips a post whose whole text is one of main's three owner lines.
The acknowledgement never answers the echo: it is a bot's message, which the acknowledgement
never takes (parent contract item 1).

**WhatsApp: R4 and R5 hold as merged; no code.** Never-silent learns that the session answered
from `notices.act` (whatsapp/adapter/src/outbound.js:285), fed by the activity
`sendWithActivity` (outbound.js:287) returns for the mayor's sends: each text chunk (:523), a
file with a caption (:652), an update template (:754), and their recovery at start. The
acknowledgement is a `gateway.send` of type reaction from src/arrival.js:161, outside the
publisher. The stopwatch learns words after the same three sends (outbound.js:525, :656, :755),
with a words-attempt marker before each (:521, :648, :752). The never-silent line goes through
`publisher.notice` (src/index.js:287, outbound.js:860-869): one `gateway.send`, with no
marker, no words line and no activity. The line is an outbound send; the acknowledgement starts
only from an owner's inbound message, so nothing in the adapter reacts to it.

## Item 2: the shared copies

`diff -rq gchat/adapter/internal/ack slack-full/adapter/internal/ack` and the same for
`internal/stopwatch`: no difference, exit 0 (every file, tests and the catalog copy
included); `cmp` of catalog.go, choice.go, service.go and stopwatch.go: equal. No file under
either directory changed in this round (`git diff --quiet 824ebab9 HEAD` over the four
directories, exit 0). `tests/test_redaction_copies.py` passes in the root run of item 5.

## Item 3: off parity at the new base (parity-base.txt)

The three files of round 2 (`gchat/adapter/internal/inbound/offparity_test.go`,
`slack-full/adapter/offparity_test.go`, `whatsapp/adapter/test/offparity.test.js`) run byte for
byte at main 205d0aec with round 2's base hooks (`../round2/base-hooks/`, unchanged: main has
no arrival service and no stopwatch, so the hooks still do nothing) and at the tip.

At main 205d0aec, round 2's files unchanged: Google Chat and WhatsApp green; Slack red on one
expectation, the state directory's journal, which holds a reply row for the root publish
(`reply@CEXAMPLE/<now>.000501`). That is main's own change, 863e9aa8 (PR 98): it journals every
session publish, root posts included. The merged tip showed the same row. The expectation now
follows main. No other expectation changed.

The NEVER_SILENT=on run, one per adapter, with `ACK_REACTION` unset and main's own never-silent
engine wired as main wires it: Google Chat as app/never_silent.go builds it and
app/workers.go hands it to the processor; Slack through `newNeverSilent`, before the company
gateway copies the config, then one reconcile and poll as `runNeverSilent`'s tick does; WhatsApp
in the adapter process itself. Besides every observation of the unset run, it reads
never-silent's state file without its times: the messages tracked and their fields, the acts,
the owners, the outages. Its expectations are main's own observations, recorded from the base
run as round 2's were. Nothing in the script fails and no health file exists, so no line is due
in either tree.

One more thing is skipped, in WhatsApp's listing only: a name `.tmp-<pid>-<hex>`, the temp file
of an atomic write (src/ledger.js `writeFileAtomic`) that the script's closing SIGKILL
interrupted. With NEVER_SILENT=on the state is written every tick, and main's own run showed
one such file. It is the kill's timing, not behaviour.

At main 205d0aec with the base hooks (parity-base.txt): Google Chat, Slack and WhatsApp each
3 of 3 tests green, the unset run, the NEVER_SILENT=on run and the start cases. At the tip the
same files are green inside each suite of item 5 (green.txt). Under heavy load the WhatsApp file
fails the same way at the base and at the tip: run at the same time, four rounds at load 56 to
72, both trees failed the same tests in every round, mostly on one extra log line when the fake
gateway missed the adapter's 600 ms inbox deadline (wa-runs.txt).

## Item 4: both switches on (red.txt)

One test per adapter, `ACK_REACTION` set to a fixed row (eyes) and `NEVER_SILENT=on`:

- Google Chat: `TestBothSwitchesOnAckIsNoAnswerAndNoticeIsNoWords`
  (gchat/adapter/internal/inbound/round3_both_switches_test.go). The arrival service and the
  stopwatch wired as the start wires them; the notice posted as `server.PostNoticeWithClient`
  posts it, `CreateNotice` on the shared client.
- Slack: `TestBothSwitchesOnAckIsNoAnswerAndNoticeIsNoWords`
  (slack-full/adapter/round3_both_switches_test.go), four routes: the switchboard DM, a company
  room, an agent DM and an MPIM.
- WhatsApp: `test/both-switches.test.js`, the adapter process against the fakes.

Each: (a) a delivered, addressed message gets the acknowledgement; the session sends no words
and no reaction; three minutes pass and fresh health says session_down; the owner line is
posted. (b) The line writes no stopwatch words line and no words-attempt marker, and its echo
(where the channel sends one back) gets no acknowledgement.

At 87778c00, before any fix: Google Chat, WhatsApp and Slack's switchboard route green; Slack's
room, agent DM and MPIM routes red on (b), one words line each. The line is posted there with
the agent's own bot token and its echo comes back through company ingress as that agent's bot
post, which round 2's `recordCompanyWords` times as words. Fixed in 16815df7: that observer
skips a bot post whose whole text is one of main's three owner lines (`neversilent.DownLine`,
`NoSeatLine`, `CapLine`). The match needs no state, holds across a restart between the post and
its echo, and also covers another city's notice in a shared company room. Its limit: a reply
made of exactly one of those three sentences is not timed. 17 lines added, 2 removed, in the
stopwatch's company observer, not in a send or arrival path.

2db97763 also feeds back the copy the channel sends of the acknowledgement before the three
minutes run: Google's `reaction.created` by the identity and Slack's `reaction_added` by the
switchboard bot. Neither is the session's reaction; the line is still posted.

To check that the tests can fail, each was also run once against scratch copies with one
mutation, never committed (red.txt has the transcripts): the acknowledgement recorded as a
session act (every adapter: owner lines 0, R4 fails); the notice writing a words line (Google
Chat, WhatsApp) or a words-attempt marker (WhatsApp): (b) fails; Google Chat's own-message
check removed from the acknowledgement path: the echo gets a reaction, (b) fails.

## Item 5: the suites (green.txt)

Every invocation, count and exit code is in green.txt. The box was shared with other sessions
at a 1-minute load of 17 to 60 for most of the round.

- Slack, 2db97763: 13,526 pass, 3 skip, exit 0. gofmt lists only company_thread_ambient.go,
  which main 205d0aec lists too; go vet clean.
- Google Chat: no full run at the tip was free of failures at these loads. Every failure was a
  wall-clock wait, and every failing test passed in another full run at the tip. Run at the same
  time as main, main's own suite passed once (1,903) and failed the same `internal/app` waits as
  the tip in a package pair at load 36 to 43; the inbound lookup hold fails 3 of 360 subtests at
  both under load. Round 1's TestNineArrivalsDeliverWhileJevIsHeld fails under load at round
  2's head too (8 of 15; the tip 4 of 15): the fake counts a request until its handler sees the
  client's cancel, while the client holds its slot until the call returns. Main's
  TestNeverSilentGateLockLoss failed at the tip in three of five full runs and passed in
  main's one full run; it was not reproduced outside a full run (0 of 12 at both at load 26 to
  33). It needs the adapter ready within about three seconds, before a seeded line falls due;
  the tip's start hashes the executable before the state lock (18 to 221 ms for the 31 MB test
  binary at load 50). NOT REPRODUCED; listed for the mayor. gofmt and go vet clean.
- WhatsApp: the three tip runs of item 6. Every test that failed at the tip passed in another
  tip run; main's suite failed in every run at the same loads.
- Root: `tests/test_stopwatch.py tests/test_reaction_catalog.py tests/test_redaction_copies.py`
  28 passed; `whatsapp/tests` 82 passed (76 in round 2: main added tests).
- No test of main or of round 2 was edited, except the three off-parity files of item 3.

## Item 6: the WhatsApp suite under load (wa-runs.txt)

Three runs at main 205d0aec and three at the tip, interleaved, each under a 900 s bound, at a
1-minute load of 12 to 53 (other sessions shared the box). Every run of both trees had a
failure: main 3 of 3 (2 failed and 1 cancelled; 1 and 1; 5), the tip 3 of 3 (4; 5; 7).

- Main's own tests failed in both trees: the read mark at first acceptance (main 2, tip 1),
  (7a) the lookup boundary (1, 1) and the 404 retry (1, 1). Others of main's failed in one tree
  only, in different runs: held-link waits and SIGKILL media cases (main: video, held C, quiet
  connection, wake and safety reads, a failed receipt's reposts; tip: audio, image, four held-link
  waits, never-silent crash retention). These are main's load-sensitive waits; reported, not fixed.
- This PR's tests that failed at the tip: the two off-parity runs (unset 1 of 3, NEVER_SILENT=on
  2 of 3), the slow-flush delivery timing check (2 of 3; 125 to 182 ms against its 100 ms median)
  and the nine-deliveries check (1 of 3). The off-parity file fails at the base too under the
  same load (the pairs above). Round 2 measured the slow-flush and nine-deliveries checks green
  in quiet runs; at this load they are load-sensitive, reported here for the mayor's ticket.

The per-run lines, every failing test per tree and the parity pairs are in wa-runs.txt.

## The self-review

One fresh reviewer on the lane's own model read the merge's resolution
(`git show --remerge-diff d17a0c23`), every later commit up to 20b8c2c0 and the PR body draft,
against R1 to R7, the parent contract's items 3, 15 and 19, main's never-silent README sections
and the public-repository rule. Counts: BLOCKER 0 / MAJOR 1 / MINOR 3 / NIT 4.

- MAJOR 1, REPRODUCED, NOT FIXED (needs a ruling). With both switches on, a mayor who answers
  with the same reaction the adapter already placed is not seen as answering. Slack: the
  acknowledgement and the mayor's react verb share the switchboard token, Slack answers the
  mayor's reaction `already_reacted`, and main's react handler then records no never-silent act
  (main.go:3320-3335, `NoticeNoActivity`; main's own test pins that rule for a repeated reaction).
  Reproduced on a scratch copy of the tip: `ACK_REACTION=eyes`, a human DM, the session reacts
  eyes; the receipt says delivered, never-silent holds no act, and after three minutes with
  session_down health one owner line is posted; with the acknowledgement off, none
  (major1-repro.txt has the test and its run). Google Chat:
  the mayor's react verb (`CreateReaction`, chat.go:362) returns an error on a 409 for a reaction
  that is already there, so the handler answers 502 and records no act (server.go:1356-1376);
  read from code, as the acknowledgement's own path expects that 409 (chat.go:413); the fake does
  not model it and it is not verified live. WhatsApp has no react verb. Every fix changes what
  never-silent counts as session activity, which this bead leaves out of scope.
- MINOR 1, PART FIXED 2db97763: the acknowledgement's echo is now fed back. Not done: Google
  Chat's test posts the line with `CreateNotice` directly, not through
  `server.PostNoticeWithClient` (a server-package test could drive it); no test restarts between
  the acknowledgement and the three minutes.
- MINOR 2, NOT FIXED: the NEVER_SILENT=on parity runs never reach the notice path (no line due,
  no outage, no mail). A second phase needs a clock seam that Slack's `newNeverSilent` does not
  have at main.
- MINOR 3, NOT SHOWN (read from code, outside this round's changes): with
  `SLACK_COMPANY_VISIBLE_ACKS` on, the company DM failure reply ("delivery failed for receipt
  ...") is posted with the owner agent's token (company_acks.go:188-198, company_dm.go:139-141)
  and its echo would be timed as words. The same class as 16815df7, older than this round.
- NIT 1, FIXED in the PR body: on WhatsApp a file without a caption is a plain send, as on main;
  the merge's commit message says "the three mayor sends" and stays as committed.
- NIT 2, FIXED 2db97763: the gchat parity comment's server.go lines.
- NIT 3, NOT FIXED: Slack's test waits 300 ms and the service for company-route goroutines it
  cannot track, so under load the "no acknowledgement on the echo" check could pass early.
- NIT 4, DOES NOT MATTER: Slack's `gofmt -l` lists company_thread_ambient.go, the same at main.

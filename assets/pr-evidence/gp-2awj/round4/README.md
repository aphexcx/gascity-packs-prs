# PR 104 round 4: main 68884bf1, and the mayor's reaction counts

Bead gp-wyes. Round 3's head is `d788822602de4dc5b6f41548a24c020e2a0c4a7e`. The new base is
main at `68884bf15089e79230bc88c55b591033e3fe927f`: c26e664e (PR 108, WhatsApp hand reactions,
quoted replies and a react verb) and 68884bf1 (PR 105, WhatsApp follow-ups) since round 3's base
`205d0aecedf944792f1e3b3fbc420b9e5ad910d8`. This round's commits, on the same branch gp-2awj:

| commit | what |
| --- | --- |
| 60a6d802 | the merge of main 68884bf1 (a merge commit; parents d7888226 and 68884bf1) |
| 4831fc4b | R2 and P2: the tests, before any fix (red.txt) |
| be5c9d75 | the acknowledgement's placement record in the shared ack package, both copies |
| ad07c56d | Slack: the session's reaction counts as with the acknowledgement off |
| ee9abca9 | Google Chat: the same |
| d7d37f62 | WhatsApp: the react verb's stopwatch line (P2) |
| 07aed5ac | R4 and P3: the README sentences |
| a71f0c3f | the self-review's M1 and m2: tests, before the fix |
| c7ac084e | M1: the placement record is written before the acknowledgement's call; the code head |

## R1: the merge

`git -c merge.conflictStyle=diff3 merge-tree --write-tree d7888226 68884bf1` and the merge itself
agree: two files conflict, three hunks, all made by PR 108 (c26e664e). PR 105 merges clean
(inbound.js, index.js, link.js and the README auto-merged; read for sense: the branch's arrival
and stopwatch wiring sits beside PR 105's held-connection switch and PR 108's `POST /react`).
Main had not moved at the claim (fetched 15:5xZ).

Every hunk keeps both sides. The only line of either side changed:

| # | file (line in 60a6d802) | main had | the branch had | kept | dropped or changed, and why |
| --- | --- | --- | --- | --- | --- |
| A | whatsapp/adapter/src/outbound.js:545-548 (sendChunks) | the request with `...(i === 0 ? quoteContext(entry.quote) : {})`, `idempotency_key: textChunkKey(key, conversationId, text, i)`, session `entry.delivered > 0 ? (entry.session_id ?? sessionId) : sessionId` | `idempotency_key: sendKey`, session `entry.session_id ?? sessionId`, then `stopwatch?.words(...)` | main's request and session rule, the branch's `sendKey` and words line | main's inline `textChunkKey(...)` became `sendKey`, which the branch computes from the same call two lines up (outbound.js:542): the same value. The branch's session expression was the old base's, not a change of the branch |
| B | whatsapp/adapter/src/outbound.js:654-720 (the file send) | the whole block: operation id, upload and `recoverOnly` inside the chain's callback in their own `try`, `...quoteContext(entry.quote)`, `quoteMetadata`, `sessionId`, the `outcome_unknown` and `window_closed` hints | two lines: the words-attempt marker before the request, the words line after the message-id check | main's block with the branch's two lines where they sat (outbound.js:680, :688) | none |
| C | whatsapp/tests/test_pack.py:77-78 | `'react': 'react.sh'` | `'stopwatch': 'stopwatch.py'` | both | none |

`git diff 68884bf1 60a6d802 -- whatsapp/adapter/src/outbound.js` is exactly the branch's own
lines against the old base (`git diff 205d0aec d7888226` on the same file): the stopwatch
parameter, three markers, three words lines and two `sendKey` constants.

### Item 2: both switches unset equals main (parity-base.txt)

The three off-parity files run byte for byte at main 68884bf1 with round 2's base hooks
(`../round2/base-hooks/`) in a git archive, NEVER_SILENT unset and on: Google Chat, Slack and
WhatsApp each 3 of 3 green. No expectation changed: neither main commit touched gchat/ or
slack-full/, and nothing the WhatsApp script observes moved. At the tip the same files are
green in each suite (green.txt).

### Item 3: main's tests and the PR's tests

No test of main or of rounds 1 to 3 was edited. Main's new WhatsApp tests (react.test.js,
quote.test.js, PR 105's link tests) and every earlier test run in the suites (green.txt,
wa-runs.txt).

### Item 4: the shared copies

`diff -rq gchat/adapter/internal/ack slack-full/adapter/internal/ack` and the same for
`internal/stopwatch`: no difference, exit 0. `cmp` of catalog.go, choice.go, service.go,
stopwatch.go and the new placed_test.go: equal. tests/test_redaction_copies.py and
tests/test_reaction_catalog.py pass (green.txt).

## R2: the mayor's reaction counts, as a class

The class, as ruled: every session reaction counts for never-silent exactly as it would with the
acknowledgement off. The acknowledgement never adds an answer and never removes one.

### The mechanism, and why

Just before the acknowledgement's channel call, the adapter records that the call may place
that emoji on that message (`ack.Service.NotePlacing`, gchat/adapter/internal/ack/service.go:173,
the same in slack-full/adapter/internal/ack). The record is an empty file `ack-placed/<digest>`
beside the attempt markers, named by the marker's own message key (`messageDigest`, :152, which
the claim now uses too) and the emoji without colons or the presentation selector. It is
dropped only when the channel answers that the call placed nothing (`DropPlacing`, :198): the
identity's reaction already there (the session's own, placed first) or a refusal. A lost or
unclear answer keeps it.

The react verb, on the channel's definite answer to the session's reaction (created, or already
there), takes the record (`TakePlaced`, :215): it removes the file, and only the first caller is
told true. An "already there" that takes it is the acknowledgement's reaction, so the session's
counts as new. A created answer takes it too, so that once the session has met the
acknowledgement's emoji, a later "already there" is the session's own repeat and main's rule
applies; the same holds when the acknowledgement's call never placed its reaction (a refused or
lost request), since the session's first reaction of that emoji is then created. While
`ACK_REACTION` is unset `TakePlaced` is false and touches nothing, whatever an earlier run left on
disk. Records older than a day are pruned with the markers (:30).

Round 4's first fix (be5c9d75) wrote the record after the channel's "created" answer. The
self-review showed that a lost answer, or a session reaction arriving while the
acknowledgement's call was in flight, then went uncounted (M1 below); c7ac084e writes it before
the call. The cost is one more file and directory sync before the acknowledgement's request.

Why not the marker alone (the READY's option A): the marker is written before the call, kept
when it fails, and says nothing of the emoji, so it would count a repeat on every acknowledged
message. The repeat cases fail it (red.txt, the scratch checks). Why the acknowledgement's record
and not a record of the session's own reactions (the spec's second shape): Slack's busy reaction
is placed with the same token (inbound_recovery.go:129), so "already there and the session never
placed it" would count a session reaction that meets the hourglass, which main does not.

- Slack: `arrivalSender` (slack-full/adapter/arrival.go:82) records before the call (:88) when
  the token is the one `handleReact` reacts with: the switchboard's (arrival.go:66) and, on
  company routes, when `ackActorToken` returns it (arrival.go:208). Slack's ok false, already_reacted
  included, drops it (:97). `handleReact` takes the record on ok or already_reacted (main.go:3329);
  a taken already_reacted is journaled without `NoticeNoActivity`, so a restart's reconcile replays
  it (never_silent_recovery.go:194). `slackArrivalSender(token)` keeps its signature and records
  nothing.
- Google Chat: `PlaceArrivalReaction` (chat/chat.go:410) takes two record calls: before the
  request with the emoji it sends (the choice, or the fallback when the choice was late; :426),
  and when Google answers 409 or another 4xx refusal, 408 aside (:433).
  `CreateArrivalReaction` keeps its signature and passes none (:400). arrival.New gives them
  (arrival/arrival.go:72, :76). arrival.Wire, which every client passes before it is shared
  (app/workers.go:48), installs `chat.Client.ArrivalPlaced` while the acknowledgement is on
  (arrival.go:93). The react verb asks it on Google's created answer or a 409
  (`chat.AlreadyThere`, chat.go:449; server/server.go:1366): a 409 that takes the record answers
  200 and records the stopwatch line and the never-silent act; any other 409 is main's 502.
- WhatsApp: no code. The acknowledgement goes to the gateway under its own key and the react
  verb under the reaction's; the fake gateway answers each with a new id, and main counts every
  delivered reaction. The class test is green at the merge and at the tip.

Never-silent's engine, causes, timings and wording are unchanged (no file under
internal/neversilent/ or whatsapp/adapter/src/never-silent.js is in this round's diff).

Limits, stated: on Slack the record is the catalog's name, so a session that reacts with
another alias of the same emoji (Slack treats aliases as one reaction) is not matched; the
channel texts give the mayor the catalog's names. A session repeat that reaches the adapter in
the moment between the channel's "already there" answer to the acknowledgement's own call and
the record's removal counts once more. Records live a day (self-review NIT 2), and the record
treats an emoji with and without the presentation selector as one (NIT 3); neither is shown.

### Per route on Slack: do the acknowledgement and the react verb share an identity?

From the code at the tip; `handleReact` always reacts with `cfg.slackBotToken` (main.go:3292).

| route | where the acknowledgement starts | its token | shared with the react verb | recorded |
| --- | --- | --- | --- | --- |
| switchboard DM | main.go:4215 (`observeSlackArrival`, no sender) | the service's Send, `cfg.slackBotToken` (arrival.go:66) | yes | yes |
| switchboard room mention (and app mentions, own threads) | main.go:4215 | `cfg.slackBotToken` | yes | yes |
| switchboard MPIM | main.go:4215 (`channel_type` mpim) | `cfg.slackBotToken` | yes | yes |
| mention-only bindings | main.go:4639, company_mention_only.go:656 | `cfg.slackBotToken` | yes | yes |
| company room | company_delivery.go:683, `ackActorToken` (company_dm.go:485) | `g.slackToken`, which is `cfg.slackBotToken` (company_delivery.go:380) | yes | yes |
| company DM | company_dm.go:110, `ackActorToken` (:479) | the owner agent's token | no | no |
| company MPIM | company_mpim.go:154, `ackActorToken` (:479) | the owner agent's token | no | no |

The spec's inference that the company routes acknowledge with the agent's own token holds for
company DMs and MPIMs; company rooms acknowledge with the switchboard token, so they share the
react verb's identity and are recorded.

### The tests

Each class test is on the start's own pieces, with ACK_REACTION fixed to eyes and
NEVER_SILENT=on, a human message addressed to the mayor and delivered:

- Slack, `TestRound4SessionReactionCountsAsWithAckOff` (slack-full/adapter/round4_reaction_counts_test.go):
  `configureArrival` and `startArrival`, `processSlackEvent` on the switchboard DM, `handleReact`;
  a local Slack stub answers already_reacted to a second identical reactions.add by the same
  token. A restart is a new engine from its state file and `reconcileNeverSilent`.
- Google Chat, `TestRound4SessionReactionCountsAsWithAckOff` (gchat/adapter/internal/app/round4_reaction_counts_test.go):
  `chat.New`, `arrival.New` and `arrival.Wire`, `server.New` and its react verb, `inbound.New`;
  the owner line goes through `server.PostNoticeWithClient` on the shared client, the call
  app/never_silent.go:44 makes. fakegoogle's new `RepeatedReactionConflicts` (opt-in, off by
  default; `TestRepeatedReactionConflictsIsOptIn`) answers 409 ALREADY_EXISTS when the same user
  already has that emoji on that message. A restart rebuilds every piece from disk, the ledger
  included, and runs `inbound.ReconcileNeverSilent` as `neverSilentGate` does.
- WhatsApp, test/round4-reaction-counts.test.js: the real adapter process against the fakes; the
  session reacts through `POST /react`; a restart is the adapter killed and started again; a
  second owner's unanswered message shows that never-silent polled past the three minutes.

The subtests, the same on Slack and Google Chat:

| subtest | what | before its fix | at the tip |
| --- | --- | --- | --- |
| class/ack-eyes-session-eyes-restart | the acknowledgement's eyes, the session's eyes, (c) the receipt (Slack delivered, Google 200), (a) a restart, three minutes, session_down: no owner line | red at 4831fc4b | green |
| class/silent-session-restart-control | the same with the session silent: the owner line is posted | green | green |
| switch-unset/old-ack-files-on-disk | (b) a run with the switch on leaves its files; the next start has it unset; the session's eyes gets main's answer (Slack delivered, Google 502), no act, the files untouched, and the owner line follows | green | green |
| lost-answer/ack-placed-answer-lost-session-eyes-twice | the channel creates the acknowledgement and its answer is lost: the session's first eyes counts, its second does not | red at a71f0c3f | green |
| in-flight/session-eyes-while-ack-in-flight-then-again | the session's eyes reaches the channel after the acknowledgement's is created and before its answer: it counts, the next does not | red at a71f0c3f | green |
| repeat/ack-eyes-session-eyes-twice | P1 (i): the first counts, the second does not | red at 4831fc4b | green |
| repeat/ack-eyes-session-thumbsup-twice | P1 (ii) | green | green |
| repeat/ack-refused-session-eyes-twice | P1 (iii): the acknowledgement's call refused, marker kept | green | green |

Slack also has `TestRound4CompanyRoomReactionCountsAsWithAckOff` (a71f0c3f): on a company room,
the acknowledgement placed with the switchboard token, the session's eyes counts once and its
second is a repeat. It was green at a71f0c3f (the record was there, untested) and fails when the
company route's record is removed.

(ii) and (iii) are green before any fix because their first reaction is a fresh placement; they
exist to fail a marker-only fix, and do. The switch-unset case fails when the gate is removed.
red.txt holds the runs before each fix and these scratch checks at the code head.

WhatsApp: the class (with the gateway seeing two reaction sends for the message), the control
and the switch unset, green at the merge and at the tip.

### The guard that stays

- Slack: `TestNeverSilentRepeatedReactionIsNotNewActivity` and `TestNeverSilentReactionJournal`,
  unedited, green at the tip (green.txt names them).
- Google Chat: `TestRound4RepeatedSessionReactionWithoutAckIsNoNewAnswer`, new beside the class:
  with the switch unset the session's eyes, then eyes again (the fake's 409): main's 502 and no
  second act. Green at 4831fc4b and at the tip.
- WhatsApp: main has no repeated-reaction rule; there is nothing to guard.

## P2: the WhatsApp react verb in the stopwatch (d7d37f62)

After a delivered placement `handleReact` records `{ event: reaction, conversation, message, at,
actor: agent }` (whatsapp/adapter/src/outbound.js:828), as the other two react verbs do; a removal
records nothing. The stopwatch keeps the first reaction per message on all three adapters, so
behind an acknowledgement the line is dropped, as it is on Google Chat and Slack. Test: the P2
case of test/round4-reaction-counts.test.js (the switch unset: a placement, a removal, a placement;
two agent lines), red at 4831fc4b, green at the tip.

## R3: the leftovers, as ruled

| leftover | outcome |
| --- | --- |
| MINOR 1's rest (a): the gchat test posts with CreateNotice | IN, done: the Google Chat class test and its control post through `server.PostNoticeWithClient` |
| MINOR 1's rest (b): no restart between the acknowledgement and the three minutes | IN, done: every class test restarts between the reaction and the three minutes, with the silent-session control |
| MINOR 2: the NEVER_SILENT=on parity runs never reach the notice path | OUT, the mayor's ticket (a clock seam in main's never-silent wiring) |
| MINOR 3: Slack's company DM failure reply under SLACK_COMPANY_VISIBLE_ACKS | OUT, the mayor's ticket (not shown) |
| NIT 3: Slack test's 300 ms settle | OUT |

## R4 and P3: the texts (07aed5ac)

Each README's "Never silent (opt-in)" section, after the sentence naming "a reaction from the
addressed session", the same words in all three: "A reaction from the session counts even when
the adapter's acknowledgement placed the same one first; the acknowledgement alone never does."

whatsapp/README.md, "Hand reactions": "Apart from the acknowledgement when `ACK_REACTION` is set,
nothing reacts automatically: this command acts only when the mayor runs it."

The three channel texts are unchanged, as ruled; tests/test_texting_rules.py and
slack-full/tests/test_slack_prompt_contract.py pass. With the fix rule 1's "A second one from you
does no harm." is true on every channel. One sentence stays as it was and is named for the mayor:
the Google Chat skill's "When `gc gchat react` fails, the reaction was not placed": the mayor's
own repeated reaction fails (Google's 409, main's 502) while the earlier one is there. That is
main's behaviour with the switch on or off; the acknowledgement no longer makes the first
reaction fail.

## The suites (green.txt, wa-runs.txt)

At the code head c7ac084e, every invocation, count and exit code in green.txt. The box was shared
with other work at a 1-minute load of 3 to 9.

- Slack: gofmt lists only company_thread_ambient.go, as main does; go vet clean. Two full runs:
  13,536 pass, 1 fail, 3 skip; then 13,537 pass, 0 fail, 3 skip, exit 0. The one failure is this
  PR's `TestOffParityWithTheBaseNeverSilentOn` (unchanged since round 3), which compares log
  lines: two lines of a coalescer retry that one of main's tests leaves running landed in its
  capture (46 such lines in each run, across unrelated tests). Reported for the mayor, not
  fixed: neither file is this round's.
- Google Chat: gofmt and go vet clean. The full run lost main's
  `TestCommandPreparationFailureSettlesExistingIntent/send-lock` (a wall-clock wait): its package
  passed at the tip and at main side by side, and the test five of five alone. The full run at
  07aed5ac lost two of main's internal/app waits, which passed beside main and alone.
- WhatsApp: three runs at main 68884bf1 (430, 429 and 431 of 431) and three at the tip (460,
  463 and 460 of 464), interleaved, Node v24.21.0. Every failure is one of main's own tests in a
  file identical to main's; every test of this PR passed in all three tip runs.
- Root: `tests/test_stopwatch.py tests/test_reaction_catalog.py tests/test_redaction_copies.py
  tests/test_texting_rules.py slack-full/tests/test_slack_prompt_contract.py` 115 passed;
  `whatsapp/tests` 107 passed.
- By name at the tip: every round 4 test, main's two Slack repeated-reaction tests
  (`TestNeverSilentRepeatedReactionIsNotNewActivity`, `TestNeverSilentReactionJournal`), round
  3's both-switches tests, the off-parity files and the ack unit test.
- No test of main or of rounds 1 to 3 was edited: after the merge this round only adds test
  files, and 14 lines to fakegoogle (the opt-in).

## The self-review

One fresh reviewer on the lane's own model read the merge's resolution
(`git show --remerge-diff 60a6d802`), every later commit up to 07aed5ac, the PR body draft and
this evidence draft, against R1 to R5, the rulings, round 2's contract items 3, 15 and 19,
main's never-silent README sections and the public-repository rule. Counts: BLOCKER 0 / MAJOR 1 /
MINOR 2 / NIT 4. It confirmed by its own reads: the merge faithful to both sides, the
switch-unset path touching nothing, never-silent's files unchanged against 68884bf1, the ack
copies byte-identical, no public-repository leak.

- MAJOR M1, REPRODUCED, FIXED (a71f0c3f the tests, red; c7ac084e the fix): the record was written
  after the channel's "created" answer, so a lost answer (Slack: the connection dropped after
  the reaction was applied; Google: the answer lost after the create), or a session reaction that
  reached the channel while the acknowledgement's call was in flight, left the session's same
  reaction uncounted, and in the second case its repeat counted. The record is now written
  before the call and dropped only on a definite "placed nothing".
- MINOR m1, FIXED in the PR body: the body stated the rule with no exceptions; it now names the
  limits (above). The README sentence stays the one sentence R4 rules.
- MINOR m2, REPRODUCED by mutation, FIXED (a71f0c3f): no test covered Slack's company room, where
  the acknowledgement uses the switchboard token and records; `TestRound4CompanyRoomReactionCountsAsWithAckOff`
  now fails under that mutation (red.txt).
- NIT 1, DOES NOT MATTER: Google Chat's counted 409 answers `{"name":"","removed":false}` where a
  created reaction carries its name; Google gave no name with the 409, and finding it would be a
  call main's react verb never makes. `gc gchat react` only prints it.
- NIT 2, NOT SHOWN (service.go:30, read from code): a record lives a day, so a session reaction
  of the acknowledgement's emoji on that message more than a day later is taken as a repeat.
- NIT 3, NOT SHOWN (service.go:160, not verifiable with fakes): the record treats an emoji with
  and without the presentation selector as one; if Google tells them apart, a session reaction
  of the other form takes the record.
- NIT 4, FIXED in part by c7ac084e: the adapter's reaction line was timed after the record's
  syncs; the record is now written before the call. On "already there" the record's removal and
  one directory sync still come before the line (a few milliseconds; not shown).

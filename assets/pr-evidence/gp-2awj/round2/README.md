# PR 104 round 2: the switch off equals main

Bead gp-w3nw. Round 1's head is `2ce179bd6a1cece2f4adb0d47f90d2183f106b99`; the PR's
own base is `c99694367081b2b3e98a58aafaf1320e091621fe`. This round's commits, on the
same branch gp-2awj:

| commit | what |
| --- | --- |
| 69ad607c | the tests of items 1 to 5 and 9 to 13, red at the head |
| 61c43e53 | the off-parity tests of item 11, with the head's hooks |
| b346134f | the shared Go packages (ack, stopwatch), both modules |
| 0e20e43c | Google Chat |
| ea4986bc | Slack |
| a377a6cb | WhatsApp |
| 1fa0e413 | self-review MAJOR 1: one chain for every stopwatch and marker flush (WhatsApp) |
| 4fe0a56d | self-review MINOR 2: a wake admitted past the reaction budget gets no reaction (Slack) |
| 0652a7a7 | the mayor's ruling on MAJOR 1's test: the chunk check made structural (WhatsApp tests only); the code head |

## Finding 1, by its class

The class: a change the switch does not gate, so that with `ACK_REACTION` unset
the adapter does not behave as it does at the base (contract item 3). The class
test is the off-parity test, one per adapter (item 11, below).

| case | where | outcome | commit | test |
| --- | --- | --- | --- | --- |
| 1a | gchat inbound.go, the identity's reaction on its own message | DROPPED: a turn again, as at the base | 0e20e43c | TestOwnReactionOnOwnMessageIsATurn; gchat off-parity turns |
| 1b | Slack arrival.go, roster drift counted and logged twice | FIXED: the arrival reads the member cache only; on a cold cache the worker starts the observation | ea4986bc | TestArrivalNeverCountsRosterDrift; Slack off-parity company healthz and log lines |
| 1c | ACK_SCOPE refused with the switch off | FIXED: checked only when the switch is set | b346134f, a377a6cb | TestScopeUncheckedWhileOff (gchat config, Slack); WhatsApp arrival case; all three off-parity start cases |
| 1d | an unhashable executable or source ends the start | FIXED: release "unknown", one warning, the adapter starts | b346134f, 0e20e43c, ea4986bc, a377a6cb | TestReleaseUnknownStartsWithOneWarning (both Go); WhatsApp stopwatch case; gchat and Slack off-parity start cases |
| 1e | Slack publish target thread reassigned | RESTORE MAIN, switch on and off | ea4986bc | Slack off-parity vendor calls (the readback's `ts`) |
| 1f | Slack root publish journaled as a reply | RESTORE MAIN | ea4986bc | Slack off-parity state directory (journal rows) |
| 1g | gchat react verb in the arrival queue | RESTORE MAIN while off; shared queue kept while on | 0e20e43c | TestReactVerbUsesBaseGateWhileOff; TestReactVerbSharesArrivalQueueWhileOn |
| 1h | ListSpaces from the arrival path; Slack files.info after an upload | RESTORE MAIN while off | 0e20e43c, ea4986bc | gchat and Slack off-parity vendor calls |
| 1i | WhatsApp marker create and flush before each send | FIXED: the send waits for the create only | a377a6cb | WhatsApp slow-flush test (send half) |
| 1j | WhatsApp stopwatch flush on the delivery's thread | FIXED: lines append off the thread; measured in item 6 | a377a6cb | WhatsApp slow-flush test (delivery half); wa-runs.txt |
| Go send paths | the Go stopwatch writes inside handlePublish, MessageCreated, the react verb, the hourglass | FIXED: Record hands the line to a writer goroutine | b346134f | TestRecordNeverWaitsOnTheDisk (both Go) |
| MINOR 3 | attempt markers never pruned | FIXED: ack markers after one day, WhatsApp words markers after 31 days, at start and daily | b346134f, 0e20e43c, ea4986bc, a377a6cb | TestOldAckMarkersArePruned (both Go); WhatsApp arrival and stopwatch cases |

## The off-parity tests (item 11)

- Google Chat: `gchat/adapter/internal/inbound/offparity_test.go`, hook `offparity_hook_test.go`.
- Slack: `slack-full/adapter/offparity_test.go`, hook `offparity_hook_test.go`.
- WhatsApp: `whatsapp/adapter/test/offparity.test.js`, hook `offparity-hook.js`.

Each test file is the same byte for byte at the base, the head and the tip. The
expected values are the base's own observations, written into the test.
`base-hooks/` holds the base's hooks at their repository paths: copy them over a
`git archive` of c9969436 together with the three test files and run there
(`base-hooks/go.mod` only keeps the root module from compiling them here).

The differences allowed, each named in the test with its contract item: the
stopwatch's files `stopwatch-YYYY-MM.jsonl` (item 15); the key-presence field
`jev_api_key` in Google Chat's status, Slack's healthz and WhatsApp's health
(item 10); the stopwatch's own warn-once lines (item 19); on WhatsApp also the
`words-attempts` directory (RULING 1 of round 1).

Where a step could not be driven as the spec names it: the Google Chat script
drives the mayor's post and three react verbs through the same `chat.Client`
calls the server's handlers make, because `server` imports `inbound` and the
test lives in `inbound`; at the head the react handler adds only a stopwatch
line. WhatsApp compares polling by acknowledged ids, not call counts, which
depend on timing.

Red at the head (red.txt), each on its own assertion: Google Chat 1a (turns,
counters), 1h (`GET /v1/spaces x2`), 1c and 1d (starts); Slack 1b (company
healthz `company_roster_drift_wakes=2`, two operator lines), 1e
(`conversations.replies ts=<root>`), 1f (a second `reply` journal row), 1h
(`files.info x1`), 1c and 1d (starts); WhatsApp 1c (start). 1g's spacing is the
same at the head; TestReactVerbUsesBaseGateWhileOff pins it.

## Item 5: no flush on a delivery or send path

- Go (both modules, byte-identical): `stopwatch.Writer.Record` keeps the dedup,
  the Slack thread normalization and the seen mark under its lock, then hands
  the line to one writer goroutine through a bounded queue of 1,024 lines
  (`stopwatch.QueueSize`). A full queue drops the line with the one existing
  warning. `Flush` waits for the queue (tests); `Close` drains it for at most
  two seconds (`stopwatch.CloseTimeout`) and runs where each adapter stops.
- WhatsApp: `record` marks the key seen and appends on one ordered
  `node:fs/promises` chain. The ack marker is created exclusively and flushed
  with its directory inside the arrival task, before the Jev call. Before a
  send the publisher waits for the words marker's exclusive create only; its
  flush runs off the send path. `flush()` for tests; a stop drains it for at
  most 1.5 seconds.
- The class test, structural (0652a7a7): `slow-flush.test.js` holds every
  stopwatch and marker flush on a named pipe, each hold occupying one thread
  of Node's pool as a stuck disk does, and requires every chunk of a long
  publish to be sent while they are held (a 20 s liveness bound, not a speed
  limit). At a377a6cb the parallel marker flushes take all four pool threads:
  2 or 3 chunks sent, 4 flushes held, the publish stalls. At the tip one chain
  holds one thread and all 14 chunks are sent (review-fixes.txt). The gaps
  between chunks, measured and kept as numbers only: 172 to 1,394 ms at
  a377a6cb with 200 ms flushes, 29 to 48 ms after the fix with the file alone,
  52 to 123 ms inside the full suite at load 7 to 35 (why a wall-clock limit
  was dropped).
- The same file still measures one delivery and one short send with each
  such flush slowed by 200 ms (the median of three under 100 ms, item 5's
  numbers). It slows the fsyncs made on stopwatch files and
  attempt markers, not every fsync: the base's own inbound
  journal flushes before the POST to the city by design, and with every fsync
  slowed the base itself measured 455 to 585 ms. `WA_SLOW_FSYNC_MATCH=.`
  restores every fsync. The test checks the preload fired (head 13 and 25
  slowed flushes, tip 7 and 11).

## The redaction (item 12)

- Google Chat: `gchat/adapter/internal/redact/` holds byte copies of
  `slack-full/adapter/redact/redact.go` and `patterns.json`, and of the shared
  cases at `testdata/cases.json`; `config.Load` sets `Redact: redact.Text`.
- WhatsApp: `whatsapp/adapter/src/redact.js` ports `redact.Text`'s span
  algorithm over `src/redact-patterns.json`, a byte copy of Slack's rules. The
  two translations at load: `(?P<token>` to `(?<token>` (jwt), and the bearer
  rule's `(?i:\bBearer[ \t]+)` to `(?:\b[Bb][Ee][Aa][Rr][Ee][Rr][ \t]+)`. All
  100 shared cases match Slack's output on both passes.
- `tests/test_redaction_copies.py` holds the five copies equal to their Slack
  sources.
- The shared fixture's "ordinary" cases include a launchd label and a host name
  that were already public in `slack-full/tests/fixtures/redaction/cases.json`;
  the copies carry them unchanged, as the byte-copy rule requires.

## The markers (MINOR 3)

At start and once a day, only empty files named by 64 hex characters, by
modification time: `<state>/ack-attempts/` older than one day (all three
adapters), `<state>/words-attempts/` older than 31 days (WhatsApp). Nothing
else in the state directory is touched, never a stopwatch file.

## The self-review

One fresh reviewer on the lane's own model read this round's delta
(`2ce179bd..a377a6cb`) against the bead, the parent contract's items 3, 15 and
19, and the public-repository rule. Counts: BLOCKER 0 / MAJOR 1 / MINOR 2 /
NIT 6. A finding was fixed only when reproduced and inside this round's own
changes, each fix a new commit with its test (review-fixes.txt).

- MAJOR 1, FIXED 1fa0e413 (reproduced). WhatsApp: a send's marker create
  queued behind the stopwatch's and the markers' flushes in Node's
  four-thread pool; the class test's preload slept on a timer and held no
  thread. Now one serial chain carries every such flush; the preload holds a
  pool thread; first a chunk-gap wall-clock check (at a377a6cb the longest gap
  was 172 to 1,394 ms, after the fix 31 to 48 ms), which the mayor replaced by
  the structural held-flush check (0652a7a7) because the gaps read up to
  123 ms under suite load.
- MINOR 2, FIXED 4fe0a56d (reproduced). Slack: the worker-started observation
  placed a reaction on a sweep's redrive long after first sight
  (TestLateRedriveTimesButDoesNotReact: 2.56 s at a377a6cb). Past the reaction
  budget it now times the message and places no reaction.
- MINOR 3, NOT SHOWN, not fixed: `slack-full/adapter/arrival.go` (the arrival
  task's call of addThreadAmbientWakes, round 1's line) reaches
  `company_thread_ambient.go:126-134`, which logs "thread-participant ledger
  scan failed" when the receipt store cannot be listed; with the switch off an
  untagged thread reply would then log it once more than at the base. Read from
  the code; not reproduced; outside this round's own changes.
- NITs, listed and not fixed: the slow-flush test asserts the median of three
  samples per measurement, not each sample (one first-send stall on a loaded
  host would otherwise fail it); TestRecordNeverWaitsOnTheDisk starts its 10 ms
  clock before the writer picks the line up (not seen to flake); a WhatsApp
  words marker whose flush fails later leaves acceptance_uncertain false, as
  RULING 1's "a send never waits for a flush" implies; existing tests build
  stopwatch Writers they never Close (the spec allows only the Flush lines in
  existing tests), one idle goroutine each; the fixture copies' public host
  name and label (above); Google Chat's `arrival.New` now runs before the state
  lock, so a second instance refused by the lock has already pruned markers
  (harmless).

## Item 6: the WhatsApp suite, measured (wa-runs.txt)

Forty whole-suite runs, interleaved base, head, tip, mix, ten each, each
bounded at 600 s, Node v24.21.0, under `env -i` with a scratch HOME; the box
was shared with other sessions (load 3.3 to 43.5). Then three quiet pairs of
base and tip at 0652a7a7 once the load fell under 10.

| tree | runs | runs with a failure | load (1-minute) |
| --- | --- | --- | --- |
| base | 10 | 4 | 5.4 to 43.5 |
| head | 10 | 3 | 3.3 to 33.7 |
| tip (4fe0a56d) | 10 | 3 | 4.4 to 35.6 |
| mix (base tests, tip source) | 10 | 4 | 5.4 to 40.8 |
| base, quiet | 3 | 1 | 7.0 to 12.0 |
| tip 0652a7a7, quiet | 3 | 0 | 6.3 to 14.5 |

Base against tip: 4 of 10 against 3 of 10; base against mix: 4 of 10 against
4 of 10. The base fails under this load too: the suite's waits are
load-sensitive on the PR's base already (its failing tests are named in
wa-runs.txt; not changed here). In these runs the head fails no more often
than the base, and the tip is at the base's rate.

## The suites (green.txt)

- Google Chat (a377a6cb; no Google Chat change after it): `env -u GC_BIN go test ./... -count=1 -timeout=30m`: 1,965 pass, 2 skip, exit 0; gofmt and go vet clean.
- Slack (4fe0a56d; no Slack change after it): `env -u GC_BIN go test ./... -count=1`: 13,376 pass, 3 skip, exit 0; go vet clean; gofmt lists `company_thread_ambient.go`, unchanged, also at the base.
- WhatsApp (0652a7a7): the toolchain wrapper refuses `GC_NO_INSTALL=1 pnpm test` (`ERR_PNPM_VERIFY_DEPS_BEFORE_RUN`: the adapter has no dependencies and no lockfile); the same test script through `pnpm exec node --test --test-concurrency=2 'test/*.test.js'` read 278 of 279 with one restore test's wait run out, and the three quiet runs of the same head read 279 of 279.
- Root (0652a7a7): `env -u GC_BIN make test PYTEST_DIRS='tests/test_stopwatch.py tests/test_reaction_catalog.py tests/test_redaction_copies.py'`: 28 passed; `env -u GC_BIN make test PYTEST_DIRS=whatsapp/tests`: 76 passed.
- `cmp` of the shared Go files (internal/ack catalog.go, choice.go, service.go; internal/stopwatch stopwatch.go) between the two modules: equal.

## What the PR body changed

The round-1 summary is kept; the sentence that the switch off changes nothing
is replaced by a section "With ACK_REACTION unset" that lists what does change
on a box; a "Round 2" section gives each case with its outcome and commit, the
off-parity tests, the redaction, the markers and item 6 in short; the Review
section lists round 1's reviews, the mayor's Fable read and round 2's
self-review; the trailers carry Bead gp-w3nw and the round-2 review lines.

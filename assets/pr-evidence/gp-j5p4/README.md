# gp-j5p4 validation and measurement

Base: `f507f7189184df2318011cfb4a6de8a82c5dc636` (main, merged PR 62).
Go implementation: `43e982a75af86f24553c1510fa98817178f14c40`; subsequent
reply-anchor changes affect Python and docs only and have their own full
Python check below. Final source and reviewed head:
`21c39771f42ea86027eee5b94625ce07e76616a0`. Evidence-only follow-up commits
do not change tested source.

## Measurement

The measure-first bead comment was submitted before implementation and before
the first commit. `measure-first.md` inventories the bot-specific rules at the
immutable base, including the discrepancy that the old ambient buffer also
admitted unlisted bots. No registry entry was changed.

`baseline.json` and `baseline-confirmation.json` reproduce the same audit
window (2026-10-14 23:00 to 2026-10-21 23:00 UTC): 575 reminders, 518,967
bytes, 20,593.93 estimated tokens/day. The only report field that changes on
the confirmation run is the number of excluded out-of-window transcript
records. `peer-counts-before.json` contains aggregate captured-copy counts;
the measure-first note separately distinguishes service-log admissions from
confirmed delivery and gives the latest-seven-day coverage limitation.

`minimal-before.json` is the production renderer at the base (source overlay),
with the legacy replay's 321,589 minimal bytes. `replay-prepare-after.json`,
`replay-after.json` and `measurement-summary.json` describe the projection
that promotes all 110 observed buffered copies to admitted peer arrivals:

| Scenario | Before bytes | After bytes | Estimated tokens/day delta |
| --- | ---: | ---: | ---: |
| Full arrival cost | 480,219 | 492,949 | +505.16 |
| Minimal renderer | 321,589 | 435,938 | +4,537.66 |

Average peer cost is 852.81 → 968.54 bytes in full and 0 → 1,040.76 bytes
in minimal. These are captured copies, not a unique-post census or bill.
Only 18 of 110 labels match the current registry; historical admission is
unknown, so no saving from excluding unlisted bots is assumed. Clipped
originals, new full-mode thread context, read costs, resolved names and actual
provider tokenization cannot be reconstructed. Raw transcripts/replay inputs
remain private in ignored lane scratch and are not committed.

## Checks

Toolchain: Go 1.27.3 darwin/arm64; Python 3.13.13 via uv with pytest, PyYAML
and jsonschema. No Node/pnpm command was needed and no unsupported-engine
warning occurred. The exact final invocations below ran from each listed
module directory. Go counts include subtests; top-level counts are provided
to distinguish those from independent test functions.

| Directory | Command | Result |
| --- | --- | --- |
| slack-full/adapter | `go test -json ./...` | 12,715 pass events (1,396 top-level), 1 skip |
| slack-full/adapter | `go test -race -json ./...` | 12,715 pass events, 1 skip |
| slack-full/adapter | `go vet ./...` | pass |
| slack-full/cli | `go test -json ./...` | 237 pass events (199 top-level) |
| slack-full/cli | `go test -race -json ./...` | 237 pass events |
| slack-full/cli | `go vet ./...` | pass |
| slack-full | `uv run --python 3.13 --with pytest --with pyyaml --with jsonschema python3 -m pytest tests -q` | 855 passed, 2 skipped, 2 existing fork deprecation warnings |
| repository root | `git diff --check f507f7189184df2318011cfb4a6de8a82c5dc636 HEAD` | pass |

The Go skip is `TestMinimalReplayMeasurement` without private replay input.
It separately passed with `INJECTION_REPLAY_INPUT` and
`INJECTION_REPLAY_REPORT`, using `go test -count=1 -json -run
TestMinimalReplayMeasurement .`. The fresh run is necessary because the test
writes its aggregate report; a cached Go test does not rewrite that file.

The final Go reports are `adapter-tests-ordering.json`,
`adapter-race-ordering.json`, `adapter-vet-ordering.json`, `cli-tests.json`,
`cli-race.json` and `cli-vet.json`. `python-anchors-final.json` records the Python exit
status; its numeric summary above is from the captured pytest report.
`completion.txt` retains all development check exits, including initial
compile/obsolete-expectation failures that were corrected before the final
runs. Raw test logs remain in ignored scratch.

Regression coverage includes both injection modes, whole Unicode bodies,
every peer arrival/thread, FIFO restore/counts and duplicate delivery claims,
attachment/path retention, held-loss dead letters, and rejected attachment
isolation through the actual coalescer ladder. Equal-length human, peer,
own-bot and unknown-bot context shares one omission rule. Readback tests
retain every author and newest 50 or 1 replies across 437 messages in three
API pages. Peer-ordering regressions cover successful and failed flush-ahead.
Reply-anchor regressions exercise the real command after a human in one
channel followed by a peer in another, for both top-level and threaded peers.
Default and explicit-turn lookups share the ordinary inbound rule; synthetic
reaction notifications remain excluded. This supersedes the earlier human-only
anchor policy noted in the pre-design inventory.

## Reviews and handoff

`self-review.md` records the fresh same-model review and follow-up rechecks:
all findings fixed, then CLEAN. External Codex rounds 1 and 2 found ordering,
pagination and reply-anchor issues; all were fixed with regressions, as
recorded in `review-response.md`. Round 3 reviewed the final source head
`21c39771f42ea86027eee5b94625ce07e76616a0` against the fixed base and
returned CLEAN. `codex-review-r3.md` contains the transcript; its `.rc` is 0
and `.reason` confirms the anchored verdict. The final follow-up commit only
records review evidence. Fable review and founder merge approval belong to
the mayor; the PR remains draft. No live adapter mode changed or restart
occurred. The queue remains in memory and oversized recovery heads stay
pending with a visible count.

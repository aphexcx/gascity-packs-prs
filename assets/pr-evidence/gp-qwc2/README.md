# gp-qwc2 evidence

Base: `f9e1dd8a217ae794337b1c4b64ad4582732302ac` (`origin/main`, PR 63).
Lane: `/Users/tailor512/city/.worktrees/gascity-packs/lane-implementation-worker-codex`.
Branch: `gp-qwc2`. The physical lane boundary and exclusive session were checked
before writes. The claim's empty branch metadata was corrected to this branch.
The pre-start BASE log was not exposed at a discoverable session path; fetched
`origin/main` and lane HEAD both resolved to the base above. No live adapter
setting, registry entry, or process was changed.

## Contract clarification

Mayor mail `ci-wisp-op0h3vv` resolved the incompatible original statements
about subtracting the protected message and retaining context under a long
message. A positive remainder is the context budget. When the protected unit
exhausts that budget, `SLACK_REMINDER_CONTEXT_FLOOR` supplies 1,200 bytes by
default, configurable as a positive integer. Every author's new message stays
whole. The marker and count can overflow pathologically tiny positive budgets.

## Replay

The fresh capture reproduces the original seven-day window (2026-10-14 23:00
through 2026-10-21 23:00 UTC): 575 reminders, 518,967 bytes. `baseline.json`
contains the aggregate measurement. The existing preparation script was
extended to retain context for full-mode replay. Batches now use the production
formatter directly, without single-message reply anchors. Its actual
path is `slack-full/scripts/replay_minimal_injections.py`, correcting the bead's
`adapter/scripts` path.

Both renderer runs use the same private JSONL input. The first baseline ran
before the composer changed. After correcting the batch harness, the baseline
was rerun with a Go source overlay of the immutable base composer and its old
omission helper. Only an unused variadic floor parameter and the helper name
were adapted for compilation; baseline composition is unchanged. The projection
promotes 110 captured buffered peer
copies to arrivals, resulting in 685 reminders / 767 members, of which 371
reminders carry captured context. Whole-window drops are **0 before and 0
after**. Full bytes are **482,843 → 482,843** (704.88 bytes/reminder); minimal
bytes are **435,938 → 435,938** (636.41 bytes/reminder). Reports are
`replay-before.json` and `replay-after.json`.

This historical sample does not reach the fitter: its 336 context-bearing
single-unit rows peak at 2,951 bytes; multi-message batches retain their
verbatim contract. Missing or previously clipped text cannot be reconstructed.
Full replay uses the verbose reply line, production batch framing, and no optional
help; minimal uses the compact reply line and the existing synthetic sender id.
The results are renderer projections, not historical delivery or billing.

Private captures, rendered inputs, and raw test logs stay in ignored
`.gc/scratch/gp-qwc2/`. Only numeric reports and synthetic fixtures are committed.
The five goldens in `slack-full/adapter/testdata/context-fit/` cover small context
under a long body, newest selection with omitted notices, CJK clipping, and
equal-length human/bot quotes. Tests also cover tiny/exact/custom floor budgets,
files-only separator accounting, compact windows, concurrency in hourly log
throttling, contradictory identities, recovery of consecutive oversized heads,
and clipping-only replay classification.

## Validation

Toolchain: Go 1.27.3 darwin/arm64; Python 3.13 through the existing uv environment
with pytest, PyYAML, and jsonschema (module paths verified before tests).
No Node/pnpm command was needed; no unsupported-engine warning occurred.
The exact commands, working directories, exit codes and Go counts are in the
individual JSON reports. `completion.txt` records completed invocations, including
the initial failing property assertion for truncated author labels, fixed before
the full suites. Python: 855 passed, 2 skipped, 2 existing fork deprecation
warnings. CLI source did not change; its required `go vet ./...` passed.

| Directory | Final invocation | Result |
| --- | --- | --- |
| slack-full/adapter | `go test -json ./...` | 12,736 pass events; 1,403 top-level; 1 replay skip |
| slack-full/adapter | `go test -race -json ./...` | 12,736 pass events; 1,403 top-level; 1 replay skip |
| slack-full/adapter | `go vet ./...` | pass |
| slack-full/cli | `go vet ./...` | pass |
| slack-full | `uv run --python 3.13 --with pytest --with pyyaml --with jsonschema python3 -m pytest tests -q` | 855 passed, 2 skipped |

Final Go reports are `adapter-tests-reviewed.json`, `adapter-race-reviewed.json`
and `adapter-vet-reviewed.json`. A later fixture-only suffix removes trailing
whitespace from the long-body golden; `goldens-final.json` records all five
goldens passing after regeneration. Production source is unchanged by that
fixture correction.

The gate's measurement-only follow-up changes no production adapter source.
Its targeted race replay command runs `TestMinimalReplayMeasurement`,
`TestReplayContextOutcome`, and `TestReplayThreadedBatchParity` (3 passed);
the baseline overlay replay passes separately (1). Python replay tests pass
(3 passed, 854 deselected). Corrected command records end in `-corrected.json`.

The optional private replay test skips without input during ordinary Go suites
and passes separately with `INJECTION_REPLAY_INPUT` / `INJECTION_REPLAY_REPORT`.

## Reviews

The required fresh same-model review ran read-only in the lane and found a
compact-window boundary bug and missing clipping-only replay counts. Both were
fixed with regressions; the final recheck was CLEAN. `self-review.md` records
the review and follow-up. The separate validated Codex gate follows the source
commit. The mayor owns the DEEP gate and Fable cross-model read on READY.

Codex DEEP round 1 reviewed source head `ab95b2cdd394f17c741aaf59b90fbbeba7457a26`
and returned CLEAN, validated exit 0, with one MINOR: the replay synthesized
single-message anchors for threaded batches. Corrected by using production
batch formatting and adding a parity test against an actual coalescer delivery.
The corrected replay totals supersede the original 485,075-byte projection.
`codex-review-r1.md` retains the finding; the follow-up gate checks its correction.

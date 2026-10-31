# gp-966g: PR 68 receipt retention, round 2

Round base: `a648f8ee2e7ff52e3b21359842c01cf803900d75` on `gp-41p6`.
The base PR contains 14 changed files, 2,201 insertions and 25 deletions
against `ec42f23490a123903fd284eec6d08752d6b29a92`, measured in this lane.

## Before-state measurement

All locations below refer to the round base. These are journal reconstruction
and in-process observer paths, with no production wire hop.

| Lifecycle point | Measured behavior | Consequence |
| --- | --- | --- |
| `ingestion_observer.go:153-157` | An accepted pending receipt enters `held`; any `ingested` or `delivered_unseen` member sets receipt-wide `concluded`. | A member verdict releases the whole receipt's timing protection. |
| `ingestion_observer.go:207-221` | Settled members are removed from pending observations; only unconcluded held receipts protect their acceptance time. | After the early member settles, neither pending observations nor receipt timing necessarily pins its evidence. |
| `inbound_journal.go:177-223` | One-day retention removes entire previous-day segments on the next append, unless pending observation/timing moves the cutoff back. Default retention is seven days. | Midnight can erase acceptance, original input interval, and settled-member evidence together. |
| `ingestion_observer.go:172-200` | A later conclusion creates a per-session row; the earlier ingestion row suppresses it only if still retained. | A previously settled member is enrolled again after pruning. |
| `main.go:4777`, `inbound_coalescer.go:3346`, `inbound_journal.go:63` | Extmsg callbacks journal their captured envelope; extmsg does not populate `injectionStartedAt`. | Reconstructed timing must retain the earlier acceptance. |
| `ingestion_observer.go:454-458` | Proof begins at reconstructed `StartedAt`, falling back to `DeliveredAt`, with 30 seconds of tolerance. | Losing acceptance excludes an earlier genuine input when conclusion is delayed by minutes. |
| `ingestion_observer.go:31` | Orphan timing is bounded by two ten-minute follow-up attempts plus the three-minute observation window: 23 minutes. | This bound and the observation window stay unchanged. |

The regression also checks the prune performed by the terminal callback:
older settled evidence must not be deleted while a newer transport row that
could re-enroll that member remains in the journal.

## Round implementation

Receipt reconstruction now distinguishes transport completion from a member's
ingestion verdict. Acceptance timing and settled-member records survive while
follow-up is active, including the first append after midnight. The existing
23-minute orphan grace and explicit follow-up termination still release them.
When pruning would keep a newer transport row but delete its member verdict,
the verdict is copied into the current segment and synced before deletion.
This retains the original timestamps, proof, canonical session and alert flag
without pinning expired segments after completion. The delayed extmsg callback
uses the preserved acceptance time; its captured envelope may omit StartedAt.

The mayor's added Fable finding extends this round. The supplied measurement
is a 60–90 minute consolidator call, versus the original controlled 2m16s
input wait. The initial alert still fires at three minutes. Alerted rows remain
observable until two hours from handoff (90 minutes plus 30 minutes of margin).
A complete match appends `ingested` with `late=true`; it supersedes the unseen
verdict without retracting or repeating the alert. Regression cases cover both
an original creation timestamp and a tool-return timestamp, restart, midnight
retention, and expiry beyond the bound.

The five requested minors are included: the public live-proof sentence and
channel/thread coordinates are redacted; round commits carry trailers;
`SLACK_INGESTION_OBSERVER_DISABLED=1` disables only the observer; a fake `gc`
executable verifies the mail argv, shared dedup key and retry after failure;
and malformed JSON lines produce a counted warning instead of disabling all
observation. A torn final fragment is separated from the next append so the
new complete row remains readable. No production process was changed.

The first lane DEEP gate at `0492bff8` returned BLOCK: disabled observation
could leave handoffs unresolved indefinitely and pin journal retention. The
follow-up fix applies ordinary configured retention while disabled, bypassing
observation reconstruction and its retention extensions. A red-then-green
regression covers midnight expiry, continued journaling and re-enabling against
the retained current-day delivery. The original verdict remains recorded in
`codex-review-r1.out`; the replacement review covers the whole round delta.

## Validation

Go 1.27.3 darwin/arm64; Python 3.13.13 with pytest, PyYAML and jsonschema
resolved in the isolated uv environment. No Node or pnpm command was used.
`completion-manifest.txt` records each command and exit; `test-results.json`
contains counts and the names of expected red failures and skipped tests.

- The final retention and long-tool regressions were replayed against the
  original production files at `a648f8ee`; all eight subcases failed for the
  expected reason (ten failures including their two parent tests). The working
  changes were restored byte-for-byte afterwards. `final-regressions-red.jsonl`
  contains the assertions and the complete run.
- `go test -race -json -run '^Test(Ingestion|InboundJournal|ReceiptFollowUp)' -count=1 .`
  in `slack-full/adapter`: 97 passes including subtests, one opt-in live skip.
  This includes the final timestamp cases and disabled-mode retention fix.
  `focused-r2-green.jsonl` records the complete run; `disabled-red.jsonl`
  records the new regression's failure before that fix.
- `go test -race -json -count=1 ./...` in `slack-full/adapter`: 1,431 top-level
  passes / 12,834 including subtests, two opt-in skips before the disabled-mode
  follow-up. The full suite is rerun for that fix as well. The earlier full run
  passed 12,832 before the two extra timestamp cases.
- The same full race command in `slack-full/cli`: 199 top-level passes / 237
  including subtests, no skips.
- `go vet ./...` in `slack-full/adapter`: passed.
- `uv run --no-project --python 3.13 --with pytest --with PyYAML --with jsonschema python -m pytest tests -q`
  in `slack-full`: 907 passed, two skipped, two existing fork-in-thread warnings.
- `git diff --check`: passed.

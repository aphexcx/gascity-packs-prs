# PR 79 round-two validation — gp-w4kz

Base: `788acecf695b79b4b23c1c5cba996fd86fd6d305`.
Final source: `4cefd2e756a537a998845427b292b428adb08d26`.
The final evidence commit does not alter source. Go toolchain:
`go version go1.27.3 darwin/arm64` (`toolchain.log`). No Node or pnpm run
was required for this Go/Python/shell pack.

## Final verification

From `gchat/adapter/`, at the final source commit:

```sh
go test ./... -count=1 -timeout=30m -json -p=1 -parallel=1
go vet ./...
gofmt -l .
```

The complete r4 run passed all 21 packages, 588 top-level tests, and 1,088
subtests, with no failures or skips. `go-test-r4.jsonl` is the full log;
`go-test-r4.summary.json` records the exact source SHA and parsed counts.
`go vet ./...` exited 0 with zero lines (`go-vet-r4.log`); `gofmt -l .`
exited 0 with zero files (`gofmt-r4.log`). All three exits were read back from
`completion.tsv` before reporting success.

From `gchat/`:

```sh
uv run --with pytest --with PyYAML --with jsonschema python -m pytest tests -q -p no:cacheprovider
```

23 tests and 22 subtests passed (10.65s), exit 0, in
`N1-review-fix-test.log`. The only later source change updates the Go launcher
test fixture. That fixture's two top-level tests and three layout subtests pass:

```sh
# From gchat/adapter/
go test ./cmd/gc-gchat-adapter -run '^TestRunScript' -count=1 -timeout=2m
```

`N1-launcher-fixture-test.log`: exit 0 (0.836s). Each run's exit status is
recorded in `completion.tsv`; full Go JSON logs have adjacent parsed summaries.
Targeted checks for each finding, including failures, are preserved in this
directory and explained in `self-review.md`.

## Earlier full runs and corrections

- r1 at `8703590d6cebb62d5da87e07cf3e234e072f86f4`: 20/21 packages passed.
  `TestPreTask15LedgerCompatibility` exposed an unwanted zero join timestamp.
  Commit `1d273d27` changes the new optional field to `omitzero`; the full ledger
  package then passed. The app and end-to-end packages passed in this run.
- r2 at `a5c2b7d2dc3ce098cb28499da71783a9f1305aec`: 19/21 packages passed,
  including all end-to-end tests. The rate-limit result was correct but took
  1.755s against an existing one-second assertion; the shutdown test missed its
  existing 500ms checkpoint assertion. Each passed five isolated repetitions
  (`rate-limit-isolated.log`, `shutdown-isolated.log`). No deadline or assertion
  was loosened. Subsequent full runs serialize packages/tests, as round one did.
- r3 at `4ad81ce409442f3a2893ca13d2a80f0a8a84f544`: 20/21 packages passed,
  including all end-to-end tests. Only `TestRunScript` and its three layout
  subtests failed, reproducing Codex r3's missing-helper finding. Commit
  `4cefd2e7` copies the real helper into the fixture's sibling commands directory.
  The run and its complete summary are retained: 587 top-level tests and 1,085
  subtests passed, one top-level test and three subtests failed, no skips.

## Codex self-review rounds

The installed `codex-gate.sh review` helper ran with `--class DEEP --model
gpt-6-astra`; each recorded result has a validated anchored verdict. Reviews did
not execute tests in their sandbox. Test results above come from this lane.

| Round | Reviewed head | Compared with | Verdict | Resolution |
| --- | --- | --- | --- | --- |
| 1 | `8703590d` | round-one head | BLOCK | Concurrent retirement could remove the HTTP handler's space snapshot. Fixed by `a5c2b7d2` with deterministic tests. |
| 2 | `a5c2b7d2` | round-one head | CLEAN, one minor | Broad inherited-export restoration could disagree with file-owned authentication mode. Fixed by `4ad81ce4`. |
| 3 | `4ad81ce4` | `a5c2b7d2` | BLOCK | Go launcher fixture lacked the new shared helper. Fixed by `4cefd2e7`. |
| 4 | `4cefd2e7` | `4ad81ce4` | CLEAN | No actionable findings in the fixture correction. |

Reports are `codex-review-r1.txt` through `codex-review-r4.txt`. These are lane
self-reviews; the mayor's gate and read follow READY and have no claimed verdict
here.

## Finding commits and scope audit

| Finding | Primary commit | Correction commits |
| --- | --- | --- |
| m4: directory timeout | `3aa49767` | |
| m5: late managed identity | `01029d07` | |
| m6: trusted provenance metadata | `aca6bae6` | |
| m7: identity/domain transaction | `dd78ca30` | |
| m8: concurrent media quota | `80ba69c1` | |
| m9: join lower bound | `ac7a10b4` | `1d273d27` |
| m1: deleted-space recovery | `26c838f7` | `a5ef6bf4`, `a5c2b7d2` |
| m2: upload budgets | `65e1c833` | |
| m3: subscription deadlines | `56a3b3a5` | |
| N1: environment precedence | `d6d986cc` | `4ad81ce4`, `4cefd2e7` |
| R1: unknown per-space 403 | `8703590d` | |

The eleven primary commits follow the requested order. Five correction commits
preserve the failed checks and their fixes without rewriting any earlier commit.
`history-audit.json` records the explicit audited source head, full commit IDs,
ancestry, author/trailer checks, and scope checks. `history-completion.tsv`
records the audit commands' exits. The round-one head is an ancestor; there are
no new merges, registry changes, other-pack changes, or dependency changes.

PR: https://github.com/aphexcx/gascity-packs-prs/pull/79 — remain DRAFT.
Final integration requires a merge commit preserving the imported history,
after Afik's merge word. This lane does not merge or mark the PR ready.

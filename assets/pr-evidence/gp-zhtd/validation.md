# Validation

Go 1.27.3 darwin/arm64. Python 3.13.13 through uv's isolated dependency
environment; pytest, PyYAML and jsonschema imports verified. No toolchain was
installed into PATH and no adapter was restarted.

- Adapter `go test -json ./...`: 1,381 top-level tests passed, 12,678 including
  subtests; one opt-in replay test skipped. This full normal run preceded the
  four self-review fixes.
- Focused tests after those fixes passed, including raw/prepared session
  rendering, ownership scanning, deletion windows, spool metadata and receipt
  conclusions.
- First full adapter `go test -race -json ./...`: **1,388 top-level tests passed,
  12,687 including subtests**, one opt-in replay test skipped; both packages
  passed, no race report.
- `go vet ./...` passed in both adapter and CLI modules; adapter vet rerun
  after review fixes. No CLI source changed.
- From slack-full: `uv run --python 3.13 --with pytest --with pyyaml --with
  jsonschema python3 -m pytest tests -q`: **822 passed, 2 skipped**, two existing
  fork-in-a-multithreaded-process deprecation warnings.
- Offline replay test explicitly enabled and passed for all 575 input blocks;
  output contains aggregate counts only. Rerun at code head 33d6eb76 produced
  321,589 bytes (60 fewer after the reaction pointer correction), or 12,761.47
  estimated tokens/day and a projected 38.03% reduction. The note and aggregate
  reports use this final replay; the earlier headline was 12,763.85/day.
- Full-mode goldens generated from the immutable 52cbd2f8 originals with a Go
  overlay, then tested against the implementation: human, thread, peer, batch.
- `git diff --check` passed.
- A final sender-coordinate edge case was fixed after the full race run:
  peer app identity can resolve without a bot user id. Minimal mode now keeps
  the bot id. `go test -race ./... -run
  'TestMinimalPeerWithoutBotUserID|TestMinimalInbounds|TestInjectionFullGoldens'
  -count=1` passed; focused reviewer pass 3 was CLEAN. Full mode is unchanged.
- After the first Codex gate's four fixes, a second full adapter race run
  passed **1,393 top-level tests, 12,694 including subtests**, one replay
  skip. Adapter vet passed again. The later cache-ordering fix passed four
  focused top-level race tests (14 with subtests), including the cold/warm
  mutation-before-remember regression, minimal paths and full-mode goldens.
- Codex R2's minor pointer-budget issue was also fixed. Nine focused top-level
  race tests passed (19 with subtests), including the budget regression,
  minimal paths, full-mode goldens and all 575 replay blocks. The replay stayed
  at 321,589 bytes. Fresh reviewer pass 6 was CLEAN.

Each batched check has a JSON completion manifest with command, working
directory, exit code, duration and available test counts. Manifests were read
before results were reported. Raw test logs remain in ignored lane scratch;
the committed evidence contains no real message text.

Fresh same-model reviewer pass 1 found four issues; all fixed. Focused pass 2
was CLEAN. Codex R1 returned BLOCK; all four findings were fixed. Fresh
reviewer pass 4 found a cache-ordering gap in those fixes; retained tombstones
address it and pass 5 was CLEAN. Codex R2 was CLEAN for the entire own delta
from 52cbd2f8 to 33d6eb76, with one minor budget finding. After fixing it,
Codex R3 was CLEAN for the incremental code, regression and measurement
refresh from 33d6eb76 to ae2b0efb; the two scopes together cover the final
implementation. The subsequent commit only records review evidence.
Fable and founder read remain the mayor's gates; this branch is delivered as
a draft PR with full mode still the default.

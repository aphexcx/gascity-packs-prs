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
- Final adapter `go test -race -json ./...`: **1,388 top-level tests passed,
  12,687 including subtests**, one opt-in replay test skipped; both packages
  passed, no race report.
- `go vet ./...` passed in both adapter and CLI modules; adapter vet rerun
  after review fixes. No CLI source changed.
- From slack-full: `uv run --python 3.13 --with pytest --with pyyaml --with
  jsonschema python3 -m pytest tests -q`: **822 passed, 2 skipped**, two existing
  fork-in-a-multithreaded-process deprecation warnings.
- Offline replay test explicitly enabled and passed for all 575 input blocks;
  output contains aggregate counts only.
- Full-mode goldens generated from the immutable 52cbd2f8 originals with a Go
  overlay, then tested against the implementation: human, thread, peer, batch.
- `git diff --check` passed.

Each batched check has a JSON completion manifest with command, working
directory, exit code, duration and available test counts. Manifests were read
before results were reported. Raw test logs remain in ignored lane scratch;
the committed evidence contains no real message text.

Fresh same-model reviewer pass 1 found four issues; all fixed. Focused pass 2
was CLEAN. Codex gate evidence is added after its committed-head review.
Fable and founder read remain the mayor's gates; this branch is delivered as
a draft PR with full mode still the default.

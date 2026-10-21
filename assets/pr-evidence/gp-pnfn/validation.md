# gp-pnfn validation

Toolchains: Go 1.26.5 darwin/arm64; Python 3.13.13 via `uv run --with pytest`; `pnpm exec node --version` reports v24.21.0 (no Node tests apply to this Go/Python change). No unsupported-engine warnings.

- `uv run --with pytest python -m pytest slack-full/tests/ -q`: 788 passed, 2 skipped; two existing `os.fork()` deprecation warnings in `test_two_process_flock_race`.
- `go test -race -json ./...` in `slack-full/cli`: 237 test/subtest passes, 0 failures, all 8 packages pass.
- `go test -race -json ./...` in repo root: pack embedding checks pass (separate module; not counted as adapter coverage).
- `go test -race ./... -run 'Test(SharedFixtures|RedactBeforeClip|InboundRedaction|SpoolLineCapIsTheProducersBound|InPlaceCompactionKeepsLinesReadable)' -count=1` in `slack-full/adapter`: both packages pass (adapter 13.712s, shared redactor 1.296s).
- Installed `.gc/184th-slack-tail.py`: the shared synthetic DM fixture through a fake `gc` reader emits the expected marker and no original token. Historical journal content was not accessed.

The complete adapter race suite passes: 12605 test/subtest passes, 0 skips, both packages pass, no race reports. Initial runs exposed a generated local attachment path being classified as base64 (fixed with structured attachment-path preservation) and oversized fixture performance (fixed with the candidate prefilter and size-preserving prose fixtures). Two intermediate full adapter runs were interrupted at the large spool test; these are not green suite evidence. The completed intermediate suite also found an existing race in a global log assertion; `publish_receipts_test.go` now uses the existing mutex-protected log buffer. Budget fixtures keep their sizes but add punctuation so they continue exercising budget limits instead of the redaction policy.


Review round 1 found three major gaps, now covered by shared regressions: JWTs overlapping base64-like URL paths, nested receipt content (link URLs, pretext, footer, field values, metadata comments), and Python Unicode word-boundary behavior for Bearer tokens. Go now merges spans like Python; the event walker scrubs every string except file download URLs; Python compiles the shared rules with ASCII semantics. The updated pinned helper passes the Unicode fixture from outside the city directory. A synthetic Slack fixture was also changed to an unmistakably fake suffix after push protection flagged its original shape; no protection bypass was used. The full affected suites pass on this revision: 12,609 adapter test/subtest passes with race detection; Python 788 passed and 2 skipped.

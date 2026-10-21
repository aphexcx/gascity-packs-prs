# gp-oaqh verification

All commands exited 0; completion manifests were read before reporting results.

- `slack-full/adapter`: `go test -race -json ./...`
- `slack-full/cli`: `go test -race -json ./...`
- `slack-full`: `uv run --with pytest --with pyyaml --with jsonschema python3 -m pytest tests -q`
- `git diff --cached --check`

Adapter: 1,375 top-level tests passed (12,676 including subtests), 2 packages.

CLI: 199 top-level tests passed (237 including subtests), 8 packages.

Python: 818 passed, 2 skipped, 2 warnings in 40.53s

Toolchain: Go 1.26.5 darwin/arm64; uv test interpreter Python 3.13.13.
Node/pnpm were not used for these Go/Python suites. No unsupported-engine
warning occurred. Python emitted two existing `os.fork()` deprecation warnings
from `test_two_process_flock_race`.

Synthetic tests cover config defaults/validation, the registered reply template,
human, threaded, peer and batch HTTP inbound envelopes, peer boundaries, UTF-8
measurement accounting, serialization deduplication, batching, aggregate privacy,
and first-context projections. Default-off behavior is covered by the existing
legacy rendering suites and explicit byte/template comparisons.

Only aggregate measurement data is committed. Raw Slack alert extracts remain in
ignored lane scratch, and no whole transcript was copied. Local command logs and
completion manifests remain in this lane's `assets/pr-evidence/gp-oaqh/`.

No adapter restart, live flag change, Slack post, profile/WAKE edit, or city helper
edit was performed. The draft PR awaits the mayor's gate and Afik's read before
activation. Core-renderer changes and session-aware context suppression are
recommendations, not implemented runtime behavior.

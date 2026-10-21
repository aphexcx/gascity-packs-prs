# gp-pnfn round 2 validation

Delta base: `03525ec56127dbe6e6301b9b5c71556b60d5c083` (PR 58's reviewed round-1 head).

The shared JWT rule now requires `eyJ` at the first segment. Dotted launchd
labels, Python module paths, local/internal hostnames, and semver suffixes
remain readable. Real-shaped JWT fixtures still redact completely; ordinary
prefixes before them now survive. The span algorithm is unchanged; its
comments were updated to describe overlaps between `eyJ` headers.

Every restored held replay's text is scrubbed before installation in memory.
A changed value marks state dirty so the next save persists the scrub. The
regression writes pre-upgrade state directly, checks the loaded and saved text,
then expires the hold and checks the LOST log for both an absent dead-letter
directory and a failed dead-letter write. Attempts, timestamps, team, and
dropped-file count are preserved. The regression fails on the previous source
at each of the load/save/LOST assertions and passes after the fix.

Commands and results:

- `go test -race -json ./... -count=1` in `slack-full/adapter`: 12,639 test/subtest passes, 0 failures, 0 skips, both packages pass, no race reports. See `round2-adapter-tests.json`.
- `go test -race -json ./... -count=1` in `slack-full/cli`: 237 test/subtest passes, 0 failures, 0 skips, all 8 packages pass. See `round2-cli-tests.json`.
- `uv run --with pytest python -m pytest slack-full/tests -q`: 800 passed, 2 skipped, 2 existing `os.fork()` deprecation warnings. See `round2-python-tests.txt`.
- `go test -race ./... -run 'TestSharedFixtures|TestLivenessRestoredHeldReplayRedactsBeforeSaveAndLostLog' -count=1` in `slack-full/adapter`: both packages pass. Red-phase output is retained in `round2-red-go.txt` and `round2-red-python.txt`; these are intentional pre-fix failures.
- `git diff --check`: pass.

Toolchains: Go 1.26.5 darwin/arm64; Python 3.13.13 through uv;
`PATH=/Users/tailor512/city/.gc/shims/toolchain:$PATH pnpm exec node --version`
reports v24.21.0. No Node tests apply and no unsupported-engine warnings occurred.
Complete Go JSONL transcripts are retained locally in `.tasks/gp-pnfn-round2/`;
the committed JSON evidence summarizes their terminal test and package events.

City integration: snapshot `4ee953c5843e63bf` contains byte-identical pack files;
all 36 shared fixtures (including idempotence) and a synthetic DM pass through
the installed module and actual wrapper from `/tmp` with fake gc. The previous
snapshot and wrapper backup remain in place. No live Slack reads or historical
journal changes were used. See `city-integration.md` and `round2-snapshot.txt`.

Self-review: checked the load-before-install ordering, dirty-state persistence,
both expiry fallback paths, preservation of replay bookkeeping, Go/Python
fixture parity, positive JWT coverage, snapshot bytes, and scope of the delta.
No additional source issue found.

Existing limitation, not introduced by this delta: a canonical Slack permalink
still matches the separately required 40+ base64 run rule. Its fixture expects
a base64 marker and no JWT marker; a short synthetic permalink stays unchanged.
Changing the base64 rule is outside the requested round-2 JWT correction. This
is recorded as papercut `pc_4fe82b567b32` for the mayor. The red-phase permalink
failure was an expectation mismatch with this unchanged rule; its fixture now
records the existing behavior explicitly.

The lane's required gpt-6-astra delta gate returned CLEAN (helper exit 0,
anchored verdict) on source commit `6117dd298d5d57abbfd98c880d025f2b04ca34ad`,
using the city helper's `review --base 03525ec56127dbe6e6301b9b5c71556b60d5c083`
mode. See `round2-codex-gate.txt`. The following evidence commit changes no pack
source. The mayor owns the subsequent delta gate and Fable delta read; the
prior Fable read was CLEAN at the base above (three minors addressed here).

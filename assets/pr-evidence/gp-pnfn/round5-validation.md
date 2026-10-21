# gp-pnfn round 5 validation

Delta base: `f4fb6efbea84379cf7157a64ac77bb1a8f31dade` (PR 58's round-4 head).

## Findings addressed

The Opus round-4 read found that an unbounded Slack workspace host could hide
a base64-shaped credential inside the permalink exception. The shared rule
now bounds the label to 1–37 characters: adding the two slashes cannot reach
the base64 rule's 40-character floor. Longer labels lose the exception. The
base64 character class still includes `/`; webhook URLs and slash-bearing
AWS secrets remain redacted. Both redactors use the same JSON without any
algorithm change. The new `slack-permalink-base64-host` shared fixture fails
in both languages on the base, then passes and remains idempotent with the
bound; all existing canonical permalink fixtures remain intact.

The path note is addressed within a small source change. File notices and
trusted candidates now use `renderedLocalAttachmentPath`. Matching only the
neutralized candidate would leave a U+200B in the printed path but not on
disk, so `safeFilename` also replaces `<` with `_` before the download creates
the file. This keeps newly downloaded filenames stable under reminder
neutralization and preserves the existing filename length bound. Every new
download also receives its message-local file index before the sanitized filename, preventing distinct names from colliding after sanitization
or redaction. Existing files are not renamed. Raw URL and raw absolute-path
candidates remain for other renderers; the neutralized candidate additionally matches old notices.

`TestInboundRedactionMarkupFilenamePathsRemainUsable` downloads a file with
`<` in its name, sends channel, partial-download fallback, and withheld copies
through the gc HTTP stub, extracts the delivered path, checks equality to the
structured on-disk path, and opens it to verify the bytes. A guard ensures
the path requires the trusted exemption; a synthetic message key still
redacts. It fails against the base with the path redacted. The existing
partial-download regression still checks the failed file is named without a
path. The repaired round-4 green transcript explicitly identifies its round-5
re-run; both rounds' green evidence includes commands, filters, and test names.

## Self-review and sibling-call audit

- The host bound is anchored to the full `https://` start, so a longer label
  cannot match a short suffix. It does not relax any provider or base64 rule.
- Only adapter-generated local attachment URLs supply trusted candidates;
  ordinary incoming text does not gain a path exemption. The filename change
  happens before disk writes and before structured attachment URLs are built.
- `mention_only.go` carries `trustedFilePaths` before clearing attachments in
  its partial-download fallback; all-success copies retain `Attachments`.
- `inbound_dead_letter.go` carries `trustedFilePaths` before withholding and
  now renders its notice with the same helper as the files block/redactor.
- `inbound_spool.go` persists `TrustedFilePaths`; `redactSpooledInbound` restores
  it before scrubbing on load and copies it on write. Structured attachments
  also supply candidates.
- `company_delivery.go`, `company_dm.go`, and `company_mpim.go` do not carry
  `trustedFilePaths`; their file hydration is snippets/metadata, without local
  downloads or a conversion that clears downloaded attachments. Their source
  is unchanged. The company mention-only path also does not download files.

The first STANDARD gate BLOCKED source `7e7d8a2b`: replacing `<` with `_`
allowed `a<b.txt` and `a_b.txt` in one message to overwrite each other. The
follow-up change prefixes **every** downloaded filename with its message-local
index, extending disambiguation beyond redacted names. Redacted names also
retain the Slack file ID so their identity survives updates. The index is before
the user-derived name, so distinct indices cannot collide even when a literal
filename resembles a generated one. Existing files and replayed stored URLs
remain untouched. `TestInboundRedactionMarkupFilenameCollisionKeepsBothFiles`
fails before this fix and passes after it, checking distinct URLs and each
file's own contents. The existing download permission, confinement, redacted-name,
and channel-delivery tests cover the resulting destinations. This remains a
small change to download naming. The second STANDARD gate BLOCKED source
`3cea39d2`: the CLI reader still used `<channel>/<ts>-<filename>`, so its
`0-report.txt` could overwrite the adapter's indexed `report.txt`.

Adapter downloads now use `<store>/<channel>.adapter/<ts>-<index>-<filename>`;
the CLI and legacy downloads keep `<store>/<channel>/`. This separates the
writers without changing CLI behavior or adding a directory depth. The
existing janitor and permission tightening inspect all directories at that
depth, so both namespaces remain managed. Only the two fixed-path assertions
and documentation needed adjustment. Existing stored paths are not migrated.

`TestInboundRedactionDownloadsShareStoreWithCLI` runs the actual Go downloader
and actual Python `spool_files` against one HTTP stub and one store, including
`report.txt` and `0-report.txt`. Only CDN-host checks are
replaced for localhost. It requires two successful CLI downloads, checks both
adapter paths still contain their original bytes, then runs the janitor
and verifies all four files and both directories are removed. It fails before
the directory separation and passes after it. The test requires Python 3;
this run executed it (zero skips), while Go-only hosts can skip this integration.
The runtime fix remains local to the downloader; no follow-up bead is needed.
Final self-review found no further issue.

## Snapshot and rollout

The city wrapper is re-pinned to `7e3a7ea9044e70b7`, the first 16 SHA-256 hex
characters over `slack_tail.py`, `slack_redact.py`, and `patterns.json` in that
order. All three installed files match the source bytes. All 51 shared
fixtures/idempotence checks pass; the actual wrapper from `/tmp` with fake gc
passes the synthetic DM, overlong-host regression, and canonical permalink.
All eight prior snapshot directories remain. The preceding wrapper is retained
as `.pre-gp-pnfn-round5`. No historical journal records were read or modified.
Adapter rollout awaits the mayor after merge; PR 58 remains draft.

## Validation

- Adapter: `go test -race -json ./... -count=1` in `slack-full/adapter`:
  12,658 test/subtest passes, both packages pass, no failures/skips/race reports.
- CLI: the same command in `slack-full/cli`: 237 test/subtest passes across
  eight packages, no failures/skips/race reports.
- Python: `uv run --with pytest python -m pytest slack-full/tests -q`:
  815 passed, 2 skipped, two existing `os.fork()` deprecation warnings.
- `git diff --check`: pass.

Raw Go JSONL remains in `.tasks/gp-pnfn-round5/`; committed summaries include
commands, working directories, exit codes and package/test counts. The focused
red/green transcripts include the exact filters and test names.

Toolchains: Go 1.26.5 darwin/arm64; Python suite 3.13.13 through uv;
the Go/Python shared-store regression invokes system Python 3.14.6;
`/Users/tailor512/city/.gc/shims/toolchain/pnpm exec node --version` reports
v24.21.0. No Node tests apply; no unsupported-engine warning occurred.

The initial adapter run before the gate finding is retained separately as
`round5-adapter-initial-tests.json` (12,656 passes). The final adapter run adds
the collision and shared-store regressions. The intermediate 12,657-pass run
is retained as `round5-adapter-second-tests.json`. CLI/Python source is
unchanged after their full runs.

## Final gate

The installed helper's gpt-6-astra STANDARD gate returned CLEAN with no
actionable findings on `f178d246f5a2bbe9f1555437a5a5557a9e2f1913`, reviewing
from `f4fb6efbea84379cf7157a64ac77bb1a8f31dade`. Exit 0 and the anchored verdict
are recorded in `round5-codex-gate-r3.txt`, `.rc`, and `.reason`. The two earlier
BLOCK verdicts and their regressions remain committed. The following evidence
commit changes no pack source; the final installed snapshot remains
`7e3a7ea9044e70b7` and was compared byte-for-byte to the final source.

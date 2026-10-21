# gp-pnfn round 4 validation

Delta base: `a8ecf813c5b561d23a95e64c753ffaeb65066478` (PR 58's round-3 head).

The mayor's round-3 gate found that `mentionOnlyInbound` retained downloaded
paths in its fallback files block but cleared the structured attachments that
gave those paths their redaction exemption. A partial download therefore lost
the successful file's usable path in the delivered reminder.

The function now copies the existing `trustedFilePaths` slice and appends each
successfully downloaded local `file:///` URL before clearing `Attachments`.
The existing files block still names the failed downloads. The redaction rules
and their treatment of untrusted message text are unchanged.

One regression downloads two attachments from a local HTTP stub: one returns
file bytes, the other returns 404. It sends the mention-only reminder through
the gc HTTP stub and verifies that the downloaded path survives, the failed
file is named without a local path, and a synthetic message credential still
redacts. The same regression fails on the base with the path replaced by a
base64 marker; `round4-regression-red.txt` records that expected failure.
`round4-regression-green.txt` records both mention-only inbound tests passing
after the fix.

## Call-site audit and self-review

- `mention_only.go`: partial-download fallback now carries `trustedFilePaths`;
  all-success delivery retains structured `Attachments`, which the redactor
  already recognizes. All-failed delivery has no downloaded paths to preserve.
  Copying the slice before append prevents changing the channel copy's backing
  array. Only structured, adapter-generated local URLs gain the exemption.
- `company_delivery.go`: `decodeCompanyMessage` scrubs stored event content and
  `commitReceipt` scrubs frozen hydration/legacy event content. Neither carries
  `trustedFilePaths` or clears a list of downloaded local attachments. Company
  file hydration stores snippets and metadata rather than local file paths.
- `company_dm.go` and `company_mpim.go`: renderers scrub text and hydration,
  then render the shared snippet/metadata file section. Neither carries
  `trustedFilePaths`; neither has the partial-download local-path conversion.
- `company_mention_only.go`: explicitly does not download files; it names all
  files with the existing unavailable-bytes notice. No trusted paths are needed.
- `inbound_spool.go`: carries `TrustedFilePaths` in its persisted envelope.
  `redactSpooledInbound` copies the in-memory provenance into that field on
  write and restores it before redaction on load. Structured attachments also
  remain available to the redactor. The existing withheld-path regression
  checks delivery before and after spool replay.
- `inbound_dead_letter.go`: `withholdAttachments` already copies and populates
  `trustedFilePaths` before clearing attachments; the new mention-only fix
  follows the same pattern. Coalescing carries those paths into its envelope.

Self-review found no other instance of this partial-download path loss in the
audited callers. No company, spool, redactor, or outbound source changed.

## Validation

- `go test -race -json ./... -count=1` in `slack-full/adapter`: 12,654 test/subtest passes, 0 failures/skips/race reports, both packages pass.
- `go test -race -json ./... -count=1` in `slack-full/cli`: 237 test/subtest passes, 0 failures/skips/race reports, all 8 packages pass.
- `uv run --with pytest python -m pytest slack-full/tests -q`: 814 passed, 2 skipped, 2 existing `os.fork()` deprecation warnings.
- `git diff --check`: pass.

Raw Go JSONL transcripts remain in `.tasks/gp-pnfn-round4/`.

Toolchains: Go 1.26.5 darwin/arm64; Python 3.13.13 through uv;
`/Users/tailor512/city/.gc/shims/toolchain/pnpm exec node --version` reports
v24.21.0. No Node tests apply and no unsupported-engine warning occurred.

The installed tail snapshot remains `12afd72318adbafc`. Its three files are
byte-identical to this round's source, so the current pin already names the
correct snapshot. All 50 shared fixtures, their idempotence checks, and the
synthetic DM through the actual city wrapper pass. All earlier snapshots
remain in place; see `round4-snapshot.txt`. No adapter rollout was performed.

The mayor owns the whole-delta round-4 gate, the cross-model delta read, and
the subsequent founder approval. PR 58 remains draft.

The lane's gpt-6-astra STANDARD delta gate returned CLEAN with no actionable
findings on source commit `48f610da3269036c3f798a03a73e9be1c939e18c`, reviewing
from `a8ecf813c5b561d23a95e64c753ffaeb65066478`. The installed helper exited 0
and validated the anchored verdict; see `round4-codex-gate.txt` and its `.rc`
and `.reason` sidecars. The following evidence commit changes no pack source.

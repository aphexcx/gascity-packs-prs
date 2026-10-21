# City integration for gp-pnfn

The authorized city helper was updated at `/Users/tailor512/city/.gc/184th-slack-tail.py`; its original is retained as `.pre-gp-pnfn`. The wrapper imports a pinned, byte-identical installation of these pack files under `.gc/slack-tail-redaction/358d1826587a5caa/slack-full/`:

- `scripts/slack_tail.py`
- `scripts/slack_redact.py`
- `adapter/redact/patterns.json`

This is an installed script snapshot, not a git checkout or worktree. No adapter restart or pack rollout was performed. The adapter fix will become active on the mayor's pack rollout after merge. No historical event records were read or changed.

The following line was appended to memory `slack-mechanics.md`:

UPDATE 2026-10-21 (gp-pnfn): `.gc/184th-slack-tail.py` now imports the pack redactor from a pinned installed snapshot and redacts secret-shaped tokens before truncating text or errors; keep WITHHOLD for sensitive partner reads. The adapter ingest fix is staged in the gp-pnfn draft PR and takes effect after the mayor rolls that pack; it redacts before journal/mail, receipt/spool and dead-letter writes, using the same pattern list. Existing journal records remain unchanged.

Final snapshot verification: all shared redaction fixtures pass against the installed module; the actual city wrapper, invoked from `/tmp` with a fake gc, redacts synthetic DM, overlapping JWT/AWS prefixes, and chained Bearer inputs. No live Slack read was used for verification.

Round 2 re-pinned the wrapper from `6849dd900e221f01` to `4ee953c5843e63bf`.
The old snapshot remains in place, and the preceding wrapper is retained as
`.pre-gp-pnfn-round2`. The new snapshot name is the first 16 hex characters of
SHA-256 over the concatenated bytes of the three files listed above, in that
order. All 36 shared fixtures, their idempotence checks, and the synthetic DM
pass through the installed module and actual wrapper; see `round2-snapshot.txt`.

Round 3 re-pinned the wrapper from `4ee953c5843e63bf` to `358d1826587a5caa`
using the same three-file SHA-256 derivation. The previous snapshots remain
in place, and the preceding wrapper is retained as `.pre-gp-pnfn-round3`.
All 49 shared fixtures, their idempotence checks, and the synthetic DM pass
through the installed module and actual wrapper from `/tmp` with fake gc;
canonical permalinks (including the thread query) survive, while webhook URLs,
slash-containing keys and adjacent secrets redact. See `round3-snapshot.txt`.

# History bounds and notices

This table was written after reading both parents' hunks, before running the
interaction tests. The merge is `0b479e5757d2036c15225019b75709abfdb8ff28`.
Its first parent is `31f5e33e8ecc103f19f1fee0ed7045a787e72dba`; its second is
`863e9aa88795101b45734f3fc0fbdcbbba2d6229`.

| File or component | Notice access | History change and result |
| --- | --- | --- |
| `internal/app/app.go` | Creates the engine after ledger load; starts the independent notice watch. | Migration precedes engine creation, so retirement must be reconciled from saved records. Both startup sequences remain. |
| `internal/app/workers.go` | Publishes the provider client before registration and passes the engine into inbound. | Floor movement stays inside each subscription retry, after registration. Both orders remain. |
| `internal/config/config.go` | Loads the switch, health path and operator settings. | The separately validated history duration remains in the same Load; neither setting masks the other. |
| `internal/inbound/inbound.go` | Tracks new addressed human messages; observes delivery outcomes, deletions and stopped attempts; reconciles at each Pass. | The history judge precedes message handling and enrollment. Old acts do not reach tracking or deletion handling. |
| `internal/ledger/ledger.go` | Stores notice resolution, delivery time, whole-paste evidence and send time. | Floor, inventory and space start fields coexist; each side's optional fields remain omitted when empty. |
| `internal/server/server.go` | Status, operator resolution, attributed replies/reactions, and notice posting read or update state. | Both status keys remain. Resolution still refuses retired records. Notice lookup now reaches StartSpace; a test compares its bound with inbound's. |
| `commands/status.sh` | Prints the never-silent object. | History prints immediately after Last event, then Never silent, then Backlog. |
| `CHANGELOG.md` | Adds the opt-in notice entry. | Both parents' entries remain at their automatically merged locations. |
| `README.md` | Explains notice enrollment, health, owner lines and mail. | Both parents' sections remain; the retirement rule will be stated beside notice enrollment. |
| `tests/test_commands.py` | Preserves reaction session attribution. | Existing history status assertions remain unchanged; the combined status test covers both keys. |
| `internal/inbound/history.go` | Does not call the engine. | Retires attempts or retains skipped queue records as committed/local with a history cause. A missing act time can use a distinct history ID. |
| `internal/catchup/history.go` | Runs before the engine exists on startup and on restore. | Installs only missing space starts, removes old queued rows, and retires unfinished attempts. Saved cursors remain untouched. |
| `internal/inbound/never_silent.go` | Replays ledger outcomes and send activity into already tracked messages. | The merge lacks a retirement case; NoticeDelivered can revive delivered evidence on a retired record. This is the repair point. |
| `internal/app/never_silent.go` | Reconciles a snapshot before every watch poll, even before registration. | Reads the migrated or restored ledger, so the same repair covers both without changing migration. |
| `internal/neversilent/engine.go` | Persists tracked messages, owner reservations and outage mail; Poll ends outages with no remaining cause. | Existing Forget creates an absent-ID tombstone. History retirement must touch only existing tracked messages to avoid one notice record per skipped act. Ordinary outage clearing and owner cooldown remain. |
| `internal/neversilent/health.go` | Reads fresh health evidence for delivered messages. | No history fields are read; retirement must take precedence before delivered evidence reaches this reader. |

State reset's existing notice-file behavior is outside this round. A main reader
recognizes a committed record with reason `local` as a non-turn in both history
rendering and forwarding decisions; it ignores the diagnostic history cause.

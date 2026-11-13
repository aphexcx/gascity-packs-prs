# Reachable self-review

A fresh read-only reviewer examined all 24 source changes at
`4e34bab05afb6f9cc7803488c818724dad6703d5` against
`e84b685a177b109877c4d47d36953e4ad85539ed`.
Initial counts: 0 BLOCKER, 7 MAJOR, 1 MINOR, 0 NIT.
A follow-up on the working changes identified four additional major cases
and one minor case within these same fault classes.

| Class | Outcome | Reproduction and fix |
| --- | --- | --- |
| Publishing a cursor before rotation continuity exists | FIXED | Empty replacement and fresh-cursor publication windows; retain the old cursor and wait for pending archives. |
| Lifecycle changes against an incomplete event projection | FIXED | Stop during backlog catch-up and deployed seq-stamped rotation; durable sequence fence. Deferred Post preserves arrivals and preexisting waits until its projection catches up. |
| Unrecoverable auxiliary lock ownership | FIXED | Dead primary/recovery/choosing owners; unique bakery tickets with dead-owner cleanup, without a reusable recovery guard. |
| Unbounded archive replay | FIXED | A 260 MiB compressed fixture replays in bounded decoded slices with persistent offsets; no entire archive is parsed in one call. A short pipe write is retried, with a deterministic reproduction. |
| Freshness compared with a historical source | FIXED | Retain old source A, invoke from changed source B; compare against the invoking pack. |
| Pack identity inferred from source spelling | FIXED | Supported Git/HTTPS/SSH/file/GitHub sources, renamed local directories, indented manifests, misleading names, and unrelated bundled imports; inspect resolved local manifests, grant nothing from an unresolved source. |
| Persisting denial accounting after measurement timeout | FIXED | Delay state publication and repeat the call; persist fail-open accounting. A process-level test uses the installed outer timeout, with cleanup time beyond the internal deadline. |
| Concurrent error-log replacement | FIXED | Interleave two error publications before rename; unique atomic error records retain both. |

The red transcripts show the initial seven failures, the interleaved error
failure, and the additional rotation, backlog, bundled-import and outer-timeout
cases. The full pack suite after these fixes passes 56 tests. Deferred call
history also respects the Stop fence that existed when the call began.

No review finding was waived. Required Codex DEEP review follows this self-review.

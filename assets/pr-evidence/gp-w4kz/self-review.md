# Round 2 self-review

## m4 — directory deadline

Reviewed the resolver's call and cache paths. The child context bounds both
request and response reads, preserves earlier caller cancellation, and records
the existing failure-cache entry without caching an empty successful address.
The stalled lookup test waits for the actual 30-second production deadline,
checks external classification, and checks the next delivery uses the failure
cache. `go test ./internal/principal -count=1 -timeout=2m` passed (30.270s).

## m5 — late managed identity

Reviewed both watcher recovery branches and the application's reload channel.
The watcher retries discovery with a cached valid token and notices discovery
from concurrent refreshes, notifying once per newly known identity. The existing
generation reload seeds and persists that identity. The app regression starts
without directory availability, then enables it and delivers a DM without a
second mint. Token/keyholder and managed-app tests passed. The first run failed
because the new test omitted the ledger's JSON field tag; r2 fixes the test and
retains both logs.

## m6 — trusted provenance metadata

Added an optional boolean to ChangeState. Missing metadata is false with no
rewrite or inference from text. Reviewed every FrameOf caller and the analogous
reaction-deletion copy: deletion only reuses marked frames; history only strips
marked frames. The legacy `[from: owner]` regressions cover deletion and history,
and existing new-record deletion/history tests remain green. Full inbound,
server, and ledger package tests passed.

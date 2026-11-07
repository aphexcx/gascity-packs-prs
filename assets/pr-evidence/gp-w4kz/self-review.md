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

## m7 — identity/domain transaction

Reviewed both credential seeding and observed-identity persistence under the
ledger mutex. A changed user clears the domain in the same save; a rejected save
restores the old user, seed, and domain. Table tests cover both paths with success
and injected persistence failures, checking disk, memory, and classification of
the previous domain's users. Related identity/reseed tests passed.

## m8 — shared media accounting

Reviewed every normalizer caller: inbound and on-demand reads enter the same
process-wide, context-aware gate before measuring usage and keep it until their
downloads finish. This avoids root-alias and reload gaps. Usage errors refuse
native downloads while retaining explanatory attachment text and Drive links.
The overlapping-reader regression stores one five-byte file under an eight-byte
cap; the unreadable-directory regression proves accounting failures fail closed.
Quota/count and new concurrency tests passed.

## m9 — join lower bound

Added an optional persisted JoinedAt separate from moving cursors. Catch-up
clamps both message and event overlap to this bound; old ledgers retain their
unknown-bound behavior. Existing space updates preserve the field. The app's
membership/history regression now places the forbidden message 30 seconds before
join, inside the old overlap, and passes with only the post-join message delivered.

## m1 — deleted-space readiness

Reviewed 404 retirement separately from authorization holds. Retirement archives
any fetched backlog as unavailable changes, removes the space interval atomically,
and reports history loss. Pass treats successful retirement as completed work.
The regression checks repeated passes and the full running-app sequence: deleted
history, readiness, then automatic recovery and recording of an interrupted send
to a live space. The first app run exposed missing service-proxy routing in the
new fixture; r2 models the city's socket proxy but still fails. The initial m1
commit incorrectly reported r2 as passing before its result was inspected. This
record is corrected without rewriting history. The fixture used a guessed city
prefix; Register actually appends ServiceURLPrefix directly. r4 uses that value
and passes both the running-app recovery and catch-up tests. r3 preserves the
failed callback diagnostic. No production correction was needed after m1.

## m2 — upload transfer budget

Reviewed interactive and automatic upload paths. The server adds one second per
256 KiB of the effective configured maximum, capped at Chat's existing 200 MiB
limit, to the 25-second command overhead. The shell uses actual file size with
45 seconds overhead, preserving the 20-second response margin. The slow 2 MiB
upload exceeds the old 25-second deadline and passes; shell tests verify the
computed curl timeout. Targeted Go tests and all 21 Python tests passed.

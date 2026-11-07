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

## m3 — subscription request deadlines

Reviewed all subscription calls, including paginated discovery and reactivation.
Each now inherits an operationTimeout child context, preserving parent
cancellation and existing ambiguous-mutation recovery. The fake-time regression
stalls get/list/create/patch/reactivate and observes the actual two-minute bound.
Its first run hung because a real HTTP keepalive goroutine prevented fake time
from advancing; a SIGQUIT trace is retained. r2 uses only scripted transport
responses inside fake time, and the full events package passes (2.309s).

## N1 — launcher environment precedence

Reviewed both shell-loading paths against config.envOr's existing nonempty
process precedence. The launchers restore inherited exports using the shell's
own `export -p` quoting, without logging or writing secret values. New file-only
variables remain loaded. A shell regression exercises service and auth with a
state path containing quotes and literal command-substitution syntax; both
preserve the value and perform no substitution. All 22 Python tests and the
config package passed. The established config precedence contract is unchanged.

## R1 — per-space authorization hold

Reviewed the boundary between inventory and per-space history: only holdAccess
changes. Unknown history 403s retain a one-hour space hold; 401 and explicit scope
failures still signal credential repair. The new test visits the held space
before the live space and confirms live delivery continues without ErrCredentials.
The first run caught a missing net/http import. The full package then exposed an
older generic-error fixture using 404; it now uses a rejected-query 400 so its
ordering assertion remains valid alongside m1 retirement. The full catch-up
package passes in r3 (36.575s), including scope and resource-denial regressions.

## m9 compatibility follow-up

The first full run exposed TestPreTask15LedgerCompatibility: `omitempty` on a
time.Time still emitted an unknown join bound. Changed only the new field's JSON
tag to `omitzero`, supported by the module's Go 1.27 toolchain. Unknown bounds are
now absent, preserving the legacy JSON shape. The full ledger package passes
(1.604s). This is an additional commit; no earlier commit was rewritten.

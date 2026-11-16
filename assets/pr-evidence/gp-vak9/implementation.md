# Join-message recovery

Bead: gp-vak9. Base: `68884bf15089e79230bc88c55b591033e3fe927f`, fetched
from origin/main. The tests-first commit is `09a7e1a1`.

The dispatch rulings are A2 and B1. This change adds no periodic work.

## Bounds and persistence

`join_reachback` is a root-level duration, default `5m`, hard maximum `60m`;
`0s` disables it. Invalid, empty or negative durations are rejected at load.

`joined_at` retains the join timestamp or discovery bound. The optional
`first_read` record carries the first message lower bound, discovery source,
time source, observation time, completion flag and pre-join message IDs while the read
is open. The existing pagination record freezes query bounds across restarts.
The completion flag prevents a second reach-back even if the first upper bound
equals the join timestamp. Existing cursor-bearing rows and open reads never
receive this state. The new top-level `last_space_list` is omitted when zero.
The existing 0.5.0 byte-for-byte load/save compatibility test stays in place.

Pre-join room messages without an identity mention are discarded before they
enter the backlog. They create no inbound change, consume no budget and send
no mail. Direct messages and mentions use the existing inbound delivery path.
Only message creations reach back; event bounds retain the join clamp.

Discovery gets membership once for each missing/bare listed room. It stages
all lookup results before saving any rows. JOINED with a usable creation time
uses the later of that time and the last recorded list; missing/invalid times,
other membership states and HTTP 403/404 fall back to the last recorded list,
or the discovery time when no list is recorded. A temporary failure aborts
the refresh and the existing retry repeats it. The lookup has a 30-second
deadline. Legacy credentials without a known identity hold this discovery
while publishing remains available to establish the identity.

## Operator log messages

- `gchat space first seen`: space resource, `found_by`, `joined_at`,
  `time_source`, `message_lower`.
- `gchat space first read complete`: space resource, `found_by`, `turns`,
  `pre_join_turns`, `elapsed_seconds`.

Discovery sources are event, startup, recovery and sweep. These records contain
no message text, display names or user IDs. The completion count reads delivered
turns from the delivery ledger and survives pending receipts and a failed save
after delivery, without maintaining a second list of every delivered turn.

## Existing tests and overlapping work

- `TestJoinedSpaceHistoryIsSweptWithoutMessageEvents` keeps its assertions and
  body: the unmentioned message before the join remains excluded.
- `TestNewSpaceUsesDefaultWake` expects discovery-time cursors instead of zero.
- `TestDelayedNewSpaceDoesNotReportHistoryLoss` uses a mention inside the
  margin; its no-history-loss assertion remains.
- `TestCatchupCadence` keeps every cadence assertion. Its private HTTP fake
  gains the one membership response required at discovery.
- `TestRetainedWindowUsesGoogleClock` models the zero-cursor case as an already
  open legacy read; bare rows now use the new discovery rule.
- The e2e fixtures supply membership times for their existing rooms. The
  downtime-join case supplies a membership and a message just after that join.
  It still tests recovery without a live message event.
- Credential-independent startup/lock tests use current credentials with an
  identity ID. A dedicated legacy-credential test preserves publishing while
  identity discovery waits.

This branch does not build on the separate first-start PR. Both changes touch
`catchup.go`'s row creation in `Freeze` (base lines 131–135) and lower-bound
clamps in `begin` (base lines 189–206). The second to land needs a rebase; its
ledger floor must also constrain this first-read lower bound.

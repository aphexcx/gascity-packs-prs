# gp-wa73 self-review, round 1, at bc0019e8

The diff against PR 107's round 5 head (db4be951) is in `gchat/adapter/internal/inbound/`:

- `coalesce.go`: the `unchanged` helper (lines 43-51) and the flush's guard (lines 233-264).
- `inbound.go`: the copy `seen` and the refresh's guard (lines 1309-1320).
- `coalesce_retirement_race_test.go` (new).

The imports added are `reflect` and `slices`. No other line changes.

Verdict: no CRITICAL, MAJOR or MINOR found by this read. Six notes:

1. NOTE, `coalesce.go:261`. A stale flush leaves `p.recoverHeld` entries in place for acts that the retirement took. Only two lines read the map:
   - `coalesce.go:220` reads it for acts that are still held, and a retired act is never held again (a retired message keeps its key but is committed).
   - `coalesce.go:342` decides whether a pass scans for held acts when coalescing is off. A leftover entry costs one snapshot per pass until the adapter restarts.

   No change.
2. NOTE, `inbound.go:695`. A guard that returns without a change still lets `update` save the unchanged ledger to disk, as main's `acceptingResult` saves do (`inbound.go:1723`).
3. JUDGMENT, `coalesce.go:261`. When the flush's guard trips, the flush picks again at once from a fresh snapshot (`continue`). It does not return and leave the acts to a later pass. The ticket says "left as the ledger now has them, for the next pass".
   - With `return`, an urgent flush from a live arrival (`admit`) gives up, and the act still held waits for the window worker's timer.
   - With `continue`, it goes out in the same call.
   - Mutant M5 (`return nil`) fails rows `member_gone_owner_still_held` and `member_redacted_under_its_key_owner_still_held`: the act still held goes out only on the next pass.
4. NOTE, `inbound.go:1316`. No test pins the refresh guard's "no longer what the snapshot saw" half; mutant M3 (existence only) passes the refresh test. The half has no reachable harm today:
   - The retirement leaves no record in intent: it commits a message's record under its key, or `preserveHistoryNotice` makes it unresolved.
   - `refreshMerged` returns false at once for any record not in intent (`coalesce_retry.go:18`).

   The compare is kept so the two guards are one rule.
5. NOTE, `inbound.go:1316`. The refresh guard compares only the owner, while the flush guard compares the owner and every member. The flush's save writes text built from the snapshot's members. The refresh's save reads every member again from the ledger under the lock, and ends the turn "stored turn is malformed" when one is missing (`coalesce_retry.go:30`).
6. NOTE, test. The hook finds the gap by function name on the stack (`flushSpace`, `refreshMerged`, `update`). A rename makes each test fail with "the retirement never ran", never pass by accident.

Public-repo check: the diff, the three commit messages and the test text carry only invented values: `spaces/room`, `users/456`, `body-002`, `edited-001`, the fixture's `examplecity`. No path, id or person's text. Done, nothing changed.

# Fresh DEEP self-review

Reviewed delta: 708ce3b87df69f6d0546d0232fe429b32d958b8b..3a5ec053556da0f1bc77147e089afec24f12f9c8.
Reviewer: fresh native Codex reviewer, same model as implementation; read-only.
Verdict: BLOCK.

P2: Google Chat can permanently consume an owner notice before credentials initialize.
The service poll in internal/app/never_silent.go:39 can run while chatClient is nil.
The engine persists Message.Notified and its owner reservation before invoking Post;
server.go:772 then rejects the nil client without attempting delivery. Existing
engine semantics retain the reservation, so later polls and restarts cannot post
after credentials recover.

Reproduction recipe: persist an overdue unreachable observation; start with
registration answering 503 and credentials unavailable for more than five seconds;
restore credentials; continue polling. Expected: one owner notice after recovery.
The reviewed code consumes the reservation and posts none. This was source-traced
by the reviewer; the implementation worker subsequently ran the regression red.

Suggested correction: an optional owner-readiness predicate checked before both
reservation writes, allowing operator mail to continue. Checking only before Post
is too late. Actual refused or uncertain provider sends retain existing semantics.

No additional in-delta findings. Normal inbound and publishing remain behind
registration. G3 omits activity for unattributed explicit replays; text, captioned
files and templates tolerate absent activity. Fresh sends and accepted original
attempts retain their attribution.

G2 activity-write reconciliation was excluded as requested. The reviewer changed
no files, ran no suites, and inspected no live state.

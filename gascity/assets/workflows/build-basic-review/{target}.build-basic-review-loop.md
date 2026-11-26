Run the build-basic starter factory review loop.

The child beads are three review lanes plus synthesis and fix application:
acceptance/correctness, test evidence, and simplicity/maintainability. These are
starter factory lanes: broad enough to demonstrate Gas City fanout/fanin, but
small enough for first-time factory users to understand.

The apply-review-findings lane owns `code_review.verdict=done|iterate` and
`code_review.report_path=<starter review summary path>`. The implementation
review check repeats this loop until the latest verdict is `done`, unless that
lane ends it first on one of its terminal exits.

From round 3 onward, a blocking finding of the same family as an earlier
round's ends the loop with `gc.build.repair_status=family_stop`, never another
fix round. Blocking findings are the synthesis's required fixes and missing
evidence; a residual risk or a PRECONDITION item is never one. Two findings
are of the same family when they name the same class of fault and the earlier
class-level fix, stated in one sentence, would close the later one too. A
different guard, input or invariant is a different fault, even in the same
file. When the lane cannot tell, it is the same family. Past round 3, a
finding is fixed only when it is reproduced (by a failing test, or shown
reachable in what this city runs) and sits in the changes made since the
previous round; the rest are listed as NOT SHOWN with their file and line.

Do not invoke provider-native subagents. Continue only through this Gas City
graph loop.


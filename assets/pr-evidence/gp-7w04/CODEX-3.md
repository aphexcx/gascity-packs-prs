# Codex DEEP round 3

Model: gpt-6-astra. Reviewed commit:
`6761019beb7c7f65d1150166bd296eef62c4eb54`.
Validated verdict: **BLOCK**. Findings: 1 MAJOR.

**Class:** Applying message expiry to identity discovery.

**FIXED:** Cold replay now includes older archives to learn historical mayor
sessions. Message expiry still applies only to waits. The red fixture covers
both archived wake records and archived identity-bearing inbound records,
followed by a recent inbound, an old session's delivered reply, and a handoff.
The answered message no longer causes a refusal.

Replay also checkpoints between small archives, so their aggregate work does
not repeatedly exhaust the per-call deadline without saving progress. A
deterministic delayed-reader fixture reproduces that failure before the fix.
The final pack suite passes 68 tests.

This is a reproduced case in the new Reachable reader introduced by this PR;
it satisfies the review policy's condition for the next review round. No
unrelated or unshown finding was used to extend the review loop.

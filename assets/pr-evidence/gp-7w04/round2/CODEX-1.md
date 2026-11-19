# Codex DEEP check 1

Reviewed SHA: `a79dbaaba54ad7058f1489310fa93d7d21ecdc88`.
Base: `59f7131451e555f7d7e5de7b1333d119513623b2`.
Verdict: BLOCK. Counts: 1 MAJOR, 0 other findings.

MAJOR at `reachable/reachable.cjs:147`: publication was matched by timestamp
rather than rotation identity. An older archive from the same second could
satisfy the legacy pending check, causing premature empty-file adoption and
loss of later inbound or outbound replay. The producer precondition is two
legacy rotations in one timestamp second; this filename format is supported.

Reproduced before fixing: warm, multiple-archive and chunked cursors, each with
pending inbound and outbound rows, all failed. A cursor ahead of the last archive
also exposed premature gap errors. The complete class is parameterized in
`reachable/test/empty-replay.test.cjs`.

Outcome: FIXED in `137ca7d6c599357f979548b58ec94484211e3f65`. Before adopting an
empty active file, the reader compares the actual snapshot tail with the
published archive tail, then checks recovered sequence continuity. Already
caught-up calls retain their existing fast path.

The reviewer performed a read-only comparison; suites were run by the worker.

# Self-review r1 of the round (gp-wf4e), at 4de5efdf

A fresh read-only subagent (general-purpose, the lane's model, Opus 5.5)
briefed with the gate record and the delta ba915531..4de5efdf only (not the
lane's conclusions), 23:12-23:23Z Wed 11/4, told to read the code rather
than the commit messages, to run tests only from copies under /tmp, and to
report CRITICAL/MAJOR/MINOR/NOTE with file:line and a failure scenario.

## Its verdict

CRITICAL 0 / MAJOR 0 / MINOR 1 / NOTE 5. VERDICT: CLEAN.

It re-ran gate.test.js from its own copies: 11 of 11 GREEN at 4de5efdf; the
same file dropped into ba915531's source: 11 of 11 RED, each for the stated
reason (MAJOR 1 "never an unmarked duplicate" x3; MAJOR 2 classifier
undefined, the fold state `received` instead of `posted`, "never an unmarked
repost" x2, "never posted again stopped holding"; MAJOR 3 gateway keys
`["K:0","K:0"]` and `["K:0","K:1","K:1"]`; MINOR "the join held at the
gateway ... stopped holding").

Its closure check, per finding (its words, condensed):

- MAJOR 1 (inbound.js:464-486) closed: the gap line is appended right after
  the detection read with no await between; only readAllWatermarks and
  clearGap reach the read or the clear and both chain on watermarkChain, so
  nothing interleaves between the two gap lines; compaction is synchronous
  and writes the fold's latest gap; an ENOSPC on the first append throws the
  read, so no plain post can follow; removing the staleness re-check adds no
  false positive (the snapshot's record at its own sequence does not change
  when an acceptance raises the watermark).
- MAJOR 2 (inbound.js:57-61, 301-318; journal.js:82-92) closed: one fsynced
  line (a torn tail drops it whole and the next sight reclassifies from the
  same inputs); the `!e.received` guard applies it once; compaction keeps an
  rid's lines in file order, so replay, compaction and the doctor agree;
  old-format journals fold as before; the startup dead-letter repair skips
  these entries (`e.dead` set).
- MAJOR 3 (outbound.js:112-113, 298, 414) closed: disjoint namespaces, keys
  of at most 75 characters, one caller key and chunk to one gateway key; the
  progress store and the CLI/gc call-back path are still keyed by the
  caller's key; nothing else looks a send up by its key.
- MINOR (inbound.js:212-240, 379) closed: bindInvite throws only on a failed
  write; every path tries the bind before the ack can be owed (first sight,
  the recovered-dead path, startup before driveAcks); only the held rid is
  filtered, status acks (null rid) and every other rid still acked;
  compaction never drops an unacknowledged entry.

## Its findings and what this round did with each

| # | Severity | Finding (file:line at 4de5efdf) | Disposition |
|---|---|---|---|
| 1 | MINOR | a join held for an owed bind shows only as "acks owed 1" (status.sh:25; binds_owed in /status JSON, printed nowhere) | FOLDED: status prints "(N held for a keeper bind)"; pytest pins it |
| 2 | NOTE | retryOwedBinds ran outside poll's try/finally; a synchronous throw would leave polling stuck (inbound.js:245-248) | FOLDED: inside the try; a non-Error keeper throw logged safely |
| 3 | NOTE (pre-existing) | the 60 s margin admits a delivery taken up to 60 s before the genesis as fresh, the unsafe direction (inbound.js:59) | COMMENT made accurate; a DESIGN QUESTION for the mayor: the other direction delays every delivery of a new journal's first minute (and every test's first delivery) |
| 4 | NOTE | with the head read after the detection, the print says "watermark 2 ahead of head 4" (status.sh:30, check.mjs:182) | FOLDED: "not in the gc store (store head M)" / "(its head unknown)"; the design's line 114 lists the same gap content and is left to the mayor's re-sync |
| 5 | NOTE (pre-existing, the MAJOR 3 class) | the progress store refuses a reused key for 7 days / 1024 keys, the gateway remembers keys 30 days: a key reused for another text after eviction gets the first message's receipt, nothing sent (outbound.js:122 vs gateway.ts:56) | FOLDED: the gateway key hashes the caller key WITH the content; RED first (node-note5-key-reuse-RED.txt) |
| 6 | NOTE (migration) | a send part-way under ba915531's `K:i` scheme, or a rollback reading `recovery` lines as fresh | NOT FOLDED: PR 74 is a draft, never deployed |

Its test check: 4de5efdf only swaps fixed pumps for pumpUntil on the same
condition, no assertion removed or loosened, (11g) adds a precondition wait.
Two caveats: the MAJOR 2 crash test's title overclaimed where the crash lands
at the head (RENAMED); (11d2)'s "B intact" check is effectively always true
because the watermarks are read A first (pre-existing at ba915531; left).

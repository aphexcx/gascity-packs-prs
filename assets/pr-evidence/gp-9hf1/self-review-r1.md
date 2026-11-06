# Self-review r1 (gp-9hf1)

A fresh read-only reviewer subagent on the lane's own model (Afik's 10/24
policy; the 349th rule), at commit `864d2f0a`, against the bead body of
record, the 11/4 deltas, the gate r7 record, the design's line 114, gc at the
fork head `f22b3ed4a` and the wecom pack. The keeper (keeper.js, zoned.js,
keeper.test.js) was out of its scope. It reproduced both MAJORs and four
MINORs in a scratch copy.

**Counts: BLOCKER 0, MAJOR 2, MINOR 11, NIT 8.** Every MAJOR and MINOR is
fixed in the head; each fix that changes behaviour has a regression in
`whatsapp/adapter/test/review.test.js`, RED against `864d2f0a`'s source
(`node-review-RED.txt`, built by `make_red_review.py`: 9 of 10 RED; the one
pass is the negative control "a delivery after the journal began is posted
plain at once").

## MAJOR

1. **A lost journal posted every held delivery plain** (inbound.js no-entry
   branch). The design: "a box that loses its journal re-reads every held
   delivery and resolves each by the lookup before posting". FIXED: the
   journal records a `genesis` line when born empty (kept through every
   compaction); a no-entry delivery the gateway received more than 60 s
   (`LOST_JOURNAL_MARGIN_MS`, clock skew) before the genesis gets a synthetic
   `posted` line (boundary 0, its gateway receipt time) and takes the in-doubt
   rule (lookup from zero, probe, marked copy or the gated plain post); a
   no-entry rid already in a dead-letter file is dead again, ack owed, never
   posted. Tests: M1 x3.
2. **A poll/ack/compaction race re-posted an acknowledged delivery** (a page
   the gateway computed before deleting it arrived after the compaction).
   FIXED: compaction drops an acknowledged rid only once a poll that BEGAN
   after its ack has completed (`pollSeq`, `ackedAtPoll`,
   `journal.compact(canDrop)`). Test: M2 (the fake gateway answers its inbox
   300 ms after computing the page; compaction every tick).

## MINOR

1. The pending deadline dead-lettered without reading the receipt. FIXED:
   the receipt is read first at every read-back; only one still pending past
   the deadline is dead-lettered. Test: Minor 1.
2. An `unsupported` verdict (a 2xx without a usable receipt) was acked.
   FIXED: no accepted line, no ack; the entry stays posted and the in-doubt
   rule decides; a concluded receipt without a usable delivery takes the
   in-doubt rule too. Test: Minor 2.
3. Phone numbers leaked through the rid (a wamid base64-encodes the number)
   in logs, status, the doctor row and the resolve verb, and gc's error body
   rode in `err.message`. FIXED: logs and every operator surface name an
   entry by `ref` (`m-` + 10 hex of the rid's sha256); `gc whatsapp inbound
   resolve` takes the ref (or the rid, hashed locally); `err.message` carries
   no response body (it stays on `err.body` / `err.json`). The (8c) test
   asserts status never names the rid.
4. A refused connection was "no answer", so a marked copy that never left
   was dead-lettered 40 s later. FIXED: a connect-phase failure
   (ECONNREFUSED, ENOENT, ENOTFOUND, EAI_AGAIN, EHOSTUNREACH, ENETUNREACH,
   UND_ERR_CONNECT_TIMEOUT) is `notSent` and retried on the ladder. Test:
   Minor 4 (the fake gc stops listening, then listens again).
5. The probe read only the first `/sessions` page (100, newest first, closed
   included). FIXED: `?state=active`, every page by `next_cursor`.
6. clear-gap raced a running watermark read, and an entry posted before a
   clear could be posted plain after it. FIXED: clear-gap runs on the
   watermark chain; a read re-checks the live watermark before writing a
   gap; an entry first posted before the last clear takes the marked path.
   Test: Minor 6.
7. A transient media failure was final (posted with a "not saved" note,
   acked, the body deleted at the gateway). FIXED: media.js flags a transient
   gateway failure; render throws before the posted line; the step retries
   on the ladder. Test: Minor 7.
8. Journal fd and short-write hazards. FIXED: compaction renames before it
   swaps the fd (a failed rename leaves the old fd on the live file); every
   line is written whole or cut back to the size before it (journal and
   dead-letter file); a closed journal refuses appends.
9. One unbounded all-or-nothing ack batch. FIXED: batches of at most 100
   ids; a batch refused with a 4xx is retried id by id. Test: Minor 9 (RED
   on the old code: one 120-id batch refused 413 forever).
10. Tests passing vacuously. FIXED: (7a), (7b) and (8b) now assert that the
    in-doubt lookup itself returned the record from the posted boundary (the
    fake gc logs what each transcript read returned). (7d)'s byte comparison
    stays: the fake builds both arms with one summary call because gc does
    (handler_extmsg.go:483-491 and 193-202 both answer
    `SummarizeInboundDelivery(receiptID, nil)`), and the test documents that
    the one-path rule is forced.
11. Fake gc fidelity. FIXED: documented that the fake models gc at
    `92a7f383f` by default and the fork head's live fan-out dedup with
    `dedupFanout`; a descending read honours `after_sequence`; an unrouted
    message appends no transcript row (inbound.go:380-384); the fake gateway
    gained inbox latency, media failures and an ack id cap.

## NIT

All taken: Rule 1's "after every poll returning a delivery" reads as a NEW
delivery (a held delivery re-read by every poll is not new; the read before
any plain post is the gate either way), stated in the README; the held row
shows the ref, receipt id and hold time; the comment on unknown journal lines
says compaction drops them; a malformed delivery is logged once; the
acknowledged line is written before the id leaves the owed set; startup is
retried on the next tick if it throws; `wa_call` curls with `--max-time`;
enroll refuses a credential that is not token-shaped before writing it into a
file run.sh sources.

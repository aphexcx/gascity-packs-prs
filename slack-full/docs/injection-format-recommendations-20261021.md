# Slack injection measurements and recommendations — 2026-10-21

Status: draft for Afik's read via the mayor. Measurements were recorded before
format design. The implementation is opt-in, default OFF; no running service or
profile has been changed. The mayor owns review and rollout approval.

## Measured facts

Snapshot: **2026-10-21 23:00 UTC**, exact seven-day interval starting 2026-10-14 23:00 UTC. Selected **55** top-level transcript files by mtime, then filtered records by timestamp. **575 injected blocks**, **518,967 UTF-8 bytes**. Token estimates use the city wake-budget rule **bytes / 3.6**, not a tokenizer. Average per 24 hours divides by seven; the first and last UTC calendar dates are partial.

The serialized inputs differ from the spec: 215 blocks occur inside user-turn wrappers and 360 in rendered delivery attachments. Queue-operation and API-request-blob copies are excluded. Unique record identity plus block hash removes copied records; distinct deliveries of the same Slack message remain counted because each costs context. Ten blocks outside the timestamp interval are excluded. No malformed JSON lines.

| Class | Bytes | Share | Bytes / block | Estimated tokens / 24h |
| --- | ---: | ---: | ---: | ---: |
| header_framing | 52,900 | 10.19% | 92.00 | 2,099.21 |
| author | 12,915 | 2.49% | 22.46 | 512.50 |
| thread_ts | 14,436 | 2.78% | 25.11 | 572.86 |
| thread_context | 102,648 | 19.78% | 178.52 | 4,073.33 |
| message | 152,948 | 29.47% | 266.00 | 6,069.37 |
| message_id | 30,225 | 5.82% | 52.57 | 1,199.40 |
| reply | 61,525 | 11.86% | 107.00 | 2,441.47 |
| peer_wrapper | 11,455 | 2.21% | 19.92 | 454.56 |
| other | 79,915 | 15.40% | 138.98 | 3,171.23 |

**Non-message share: 70.53%. Total estimated tokens / 24h: 20,593.93.** Non-message bytes include useful context and identifiers; this is not a claim that all are removable.

The author class is the outer name/id prefix (and any bracketed clause before its colon). The observed `[thread reply …; parent …]` anchor follows that prefix and is counted in `other`, along with coalescing directions and the once-per-channel help block. `thread_context` includes re-quotes, already-delivered notices, and their separator; `thread_ts` is the separate `[slack thread_ts=…]` line. Peer wrapper counts only its framing; peer message content remains message text. Footer separators belong to `message_id`; header separators belong to `header_framing`. Every UTF-8 byte is assigned exactly once.

## Batching and repetition

| Reminders in one serialized user/attachment record | Records |
| ---: | ---: |
| 1 | 571 |
| 2 | 2 |

A separate observable proxy for a mayor turn groups delivered records before the next assistant record (not a claim about provider prompt-cache billing):

| Reminders before next assistant record | Groups |
| ---: | ---: |
| 1 | 446 |
| 2 | 42 |
| 3 | 7 |
| 4 | 3 |
| 6 | 2 |

| UTC date | Blocks / Reply occurrences | Distinct exact Reply lines | Repetitions after first | Largest identical group |
| --- | ---: | ---: | ---: | ---: |
| 2026-10-14 | 4 | 1 | 3 | 4 |
| 2026-10-15 | 69 | 22 | 47 | 19 |
| 2026-10-16 | 53 | 16 | 37 | 29 |
| 2026-10-17 | 94 | 86 | 8 | 4 |
| 2026-10-18 | 5 | 5 | 0 | 1 |
| 2026-10-19 | 130 | 30 | 100 | 52 |
| 2026-10-20 | 89 | 23 | 66 | 29 |
| 2026-10-21 | 131 | 44 | 87 | 33 |

The Reply command template is identical in all 575 blocks; the table counts exact repeats including channel/thread IDs, not just the shared command syntax.

## Recommendations (ANALYSIS)

**Start with the Reply line:** register
`Reply IDs: channel=<channel> thread=<root-or-message-ts>` instead of the
repeated shell command. This single registration-template change saves **52
bytes per block** and preserves both explicit reply coordinates. At the
observed volume it saves **1,186.51 estimated tokens per 24h** (5.76%), lowering
the estimate from **20,593.93 to 19,407.42 tokens per 24h**. It is the largest
saving among the simple single-template substitutions.

Fresh sessions would lose the local command reminder. Enable only for sessions
whose profile already supplies the command contract. A real once-per-session
introduction belongs in the session harness and must replay after context
reset/compaction. The existing `replyHelpBlock` is once per channel per
**adapter lifetime**, not once per agent session; it cannot establish that a
new session knows the command. This bead does not edit profiles or WAKE.

The table is a counterfactual replay of the same 575 blocks, not deployed
traffic or billed-token measurements. Per-block savings average over **all**
blocks. Recovering omitted context costs additional tokens, excluded here.
Savings do not overlap; the first-context policy uses transcript session IDs,
not an adapter-wide cache.

| Class | Decision | Bytes saved / block | Estimated tokens saved / 24h | Loss and recovery |
| --- | --- | ---: | ---: | --- |
| Reply instruction | Shorten to explicit channel/thread IDs | 52.00 | 1,186.51 | Loses local command help; recover from session-start contract/profile, or `gc slack reply-current --help`. Implemented behind the flag. |
| Header + framing | Shorten `New message in shared conversation slack/` to `Slack `; keep both `system-reminder` boundaries | 35.00 | 798.61 | Loses shared-conversation prose; source and channel remain. Requires gc renderer change. Framing saves zero because it stays. |
| Author + bracketed clause | Keep name/id and provenance | 0.00 | 0.00 | No identity loss. The author-adjacent thread anchor is accounted for under `other`, as above. |
| thread_ts line | Shorten `[slack thread_ts=<root>]` to `thread=<root>` | 7.67 | 175.04 | Loses provider wording, still present in header. Root remains explicit. Proposal only. |
| Thread context | Keep first context per session/channel/thread; omit subsequent blocks | 111.83 | 2,551.63 | Can lose newly fetched replies or context forgotten after compaction. Recover with `gc slack read --conversation-id <channel> --thread-ts <root>`. Requires session/compaction-aware state and delivery acknowledgements. Proposal only. |
| Message id | Shorten `Message id: ` to `ts=` and `(in thread ` to `(thread=` | 10.96 | 250.00 | Loses verbose labels only; timestamp and root remain. Requires gc renderer change. |
| Peer wrapper | Shorten header to `[peer-bot context: read-only; not addressed to you; do not reply or act]`; retain footer | 6.73 | 153.61 | No provenance or instruction loss. Closing boundary keeps a following human ask outside the read-only block. Implemented behind the flag. |
| Other | Shorten thread-anchor prefix to `[thread=` while retaining parent text; shorten batch directions | 30.75 | 701.75 | Parent text and every batch member's author/ts/thread/body remain; full CLI syntax lives in profile/help. Only batch directions are implemented behind the flag. |

The context replay retained **114 first contexts** and removed **284 later
contexts**. This is a savings ceiling for that policy, not proof later blocks
are redundant. Current code already deduplicates delivered messages and often
emits only an “already delivered” notice; blanket deletion would also remove
some unseen context. Clearing state on session replacement and compaction,
preserving a marker, and on-demand reads would be necessary before adopting it.
Current thread-context delivery stays intact in the opt-in implementation.

A literal one-line peer wrapper is suitable only when each peer post is a
separate scoped record. Buffered peers precede human text inside one record;
removing the footer would blur the scope of “do not act.” Keep that boundary
and shorten the header.

| Scenario | Estimated tokens / 24h | Savings vs measured baseline |
| --- | ---: | ---: |
| Measured baseline | 20,593.93 | — |
| Smallest recommendation: Reply IDs only | 19,407.42 | 1,186.51 (5.76%) |
| Implemented opt-in: Reply IDs + peer header + batch directions | 18,995.99 | 1,597.94 (7.76%) |
| Full proposed set, including session-aware context and gc renderer changes | 14,776.79 | 5,817.14 (28.25%) |

The full-set projection saves **146,592 bytes** over the sample; the implemented
flag saves **40,268 bytes**. Provider tokenization, prompt caching, future
traffic, context recovery, and service versions will affect realized savings.

## Implementation and rollout boundary

`SLACK_COMPACT_ALERTS=on` is the new setting. Unset/`off` takes the original
branches and registers the original template byte-for-byte. It changes
registration and immediate/buffered peer and coalesced-batch presentation.
Single human/thread payloads, structured inbound IDs, routing, full help,
budget rules, and delivery state remain intact. Tests cover human, thread,
peer, and batch envelopes through the HTTP inbound seam, plus configuration,
registration, and peer boundaries. The core renderer is not part of this pack
and is not replaced by a test-only imitation.

Core gc owns the outer header, author line, Message id footer, and
`system-reminder` framing. Its adapter-registration `reply_instructions` field
is the supported seam for the Reply line. Empty templates may use a generic
fallback, so compact mode registers a **nonempty** IDs template. Separate
alias, mention-only, and company renderers are outside this measured
shared-conversation family and remain unchanged.

No deployment, restart, or setting change is part of this bead. The draft PR
prepares an opt-in for review. Activation and broader changes require Afik's
read relayed by the mayor. Rollback is `off` with adapter re-registration on
an approved rollout. SIGHUP alone cannot update this startup flag or template.

Read-only inspection confirmed `.gc/184th-slack-tail.py` delegates to pinned
`slack_tail.py`, which invokes `gc slack read --json`.
`assets/ops/mayor-tools/founder-gap-sweep.sh` also invokes `gc slack read --json`
for channel/thread history; `founder-gap-emit.py` reads its JSON message fields
and `reactions.get`. None parses injected reminder text, so these presentation
changes do not affect them. No city `.gc` or helper file was modified.

## Reproduction

Run from a worker lane; raw extracts remain in its ignored `.gc/scratch/` directory. The report contains aggregate numbers only. No transcript message text is included in this note. The committed aggregate snapshot is `assets/pr-evidence/gp-oaqh/measurement.json`. Later records in live transcript files can increase the out-of-window exclusion count on a rerun; the timestamp window stays fixed. The script also reproduces the counterfactual projections.

```sh
python3 slack-full/scripts/measure_injected_alerts.py \
  --transcripts /Users/tailor512/.claude/projects/-Users-tailor512-city \
  --scratch .gc/scratch/gp-oaqh \
  --report .gc/scratch/gp-oaqh/measurement.json \
  --as-of 2026-10-21T23:00:00Z
```

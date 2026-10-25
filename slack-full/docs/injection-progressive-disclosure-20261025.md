# Slack injection: progressive disclosure, 2026-10-25

Draft for the founder's first-head read. Built on Sinan's jg-fc1csv commits
`84b8da44`, `910c232b`, `52cbd2f8`, unchanged. No live adapter setting or agent
prompt has been changed. Full is the default.

## Measure first

The pre-design bead comment records the live read and two extracted reminders.
Only those blocks were extracted into ignored lane scratch; mayor transcripts
were read-only. The source thread is `C0ASAPRETDK / 1792622843.244239`.

| Observation | First reply | Second reply |
| --- | ---: | ---: |
| Earlier messages in Slack at that timestamp | 6 | 9 |
| Earlier messages quoted in the reminder | 4 | 2 |
| Complete reminder bytes | 2,803 | 1,312 |
| Explicit already-delivered count lines | 0 | 0 |
| Messages lost to the 20-message fetch limit in this case | 0 | 0 |

The later live read returned 13 messages; later posts are not counted as
missing priors. The second reminder quotes the 332-character root and one
peer post: 281 characters / 283 bytes including ellipsis. Its original text is
1,693 characters / 1,697 bytes. Newline collapse makes that 1,700 characters /
1,704 bytes; the 280-byte retained prefix removes 1,420 collapsed characters /
1,424 bytes. The first reminder's two peer quote prefixes remove 300 and 1,511
collapsed characters (300 and 1,529 bytes).

**ANALYSIS: missing recent context.** `threadContextCache.lastDeliveredFor`
and `markDelivered` in `adapter/thread_context.go`, called by
`processSlackEvent` in `adapter/main.go`, track a target/channel/thread
watermark for the adapter lifetime. `formatThreadContextPreamble` omits
non-root messages at or before that watermark and filters adapter-self posts.
Using the first inbound timestamp as the second inbound's watermark predicts
the observed second view: six non-root priors at/below the watermark, one
intervening adapter-self post omitted, root and newest peer quoted. A session
reset does not reset this target cache. The historical cache was not persisted,
so its exact key/value cannot independently be measured; the normal channel
audience has empty target. This is distinct from `deliveredIDs` collapsing
quoted candidates into a count: neither extract has that count line.

**FACT: shortened peer text.** `threadContextBotPostBytes = 280` and
`clipBotPost` in `adapter/thread_context.go` explain the observed quote exactly.
The buffered-peer limit in `adapter/peer_bots.go` is 2,000 **bytes**, despite
its `Chars` name. All three matched peer originals are below it: it removed
zero bytes here. Its 20-item FIFO, the 1,400-byte total bot-quote allowance,
and the 3,500-byte reminder budget do not explain these extracts. The anchor
separately keeps only the parent's first 120 runes. These are intentional full
mode presentation limits, retained and documented; on-demand reads recover
the remaining text. The first bead comment's 6,000-byte budget was corrected
to the source's 3,500 bytes before implementation was completed.

**Separate bug, fixed:** `fetchThreadReplies` requested only the first
oldest-first page. With 25 messages and limit 20, the newest five were absent.
It now follows cursors, retaining the root plus the newest 19 replies in bounded
memory; current and later messages are excluded before selecting a window.
Incomplete pagination fails visibly rather than returning a misleading window.
Ownership routing scans use a separate paginated existence query, so an early
own-bot post remains evidence even when outside the newest context window.
The short-thread golden fixtures remain byte-identical. Regression tests in
`injection_mode_test.go` reproduce the delta cache, both clip sizes and the
25-message window, including a historical inbound's upper bound.

Source references at the immutable `52cbd2f8` base:
`adapter/thread_context.go:126` (cache read), `:206` (cache advance), `:623`
(watermark filter), `:308` / `:747` (280-byte clip), `:535` (single-page fetch);
`adapter/main.go:4242` / `:4291` (target-scoped read/advance);
`adapter/peer_bots.go:255` / `:352` (buffer clip);
`adapter/reminder_budget.go:48` / `:54` (3,500-byte budget / 120-rune anchor).

## Synthesis of the two audits

Citadel's byte accounting uses 575 blocks from seven days ending
2026-10-21 23:00 UTC, estimated tokens = UTF-8 bytes / 3.6 / 7. Sinan's audit
uses a different sample and cl100k_base; its token counts are not added to
Citadel's. Entries marked “included” are subdivisions, not extra bytes.

| Boilerplate class | Citadel bytes/block | Est. tokens/day | Sinan's branch | PR 59 | Synthesis |
| --- | ---: | ---: | --- | --- | --- |
| Header and framing | 92.00 | 2,099.21 | Unchanged | Recommend shorter gc header | gc follow-up; unchanged here |
| Author | 22.46 | 512.50 | Keep | Keep | Keep display name and id |
| Thread anchor | Included in other | Included in other | Shorter anchor, no stale flag | Recommend shorter label | Keep thread id; cached parent author/first line |
| Separate thread_ts marker | 25.11 | 572.86 | Removed | Recommend shortening | Already removed |
| Thread context re-quotes | 178.52 | 4,073.33 | Shorter collapsed-priors tail | Recommend session-aware suppression | Minimal: no re-quotes; read thread on demand |
| New message including peer items | 266.00 | 6,069.37 | Keep; smaller peer clips proposed | Keep | Keep new message; omit buffered peer context |
| Message id/footer separators | 52.57 | 1,199.40 | Unchanged | Recommend shorter gc label | gc follow-up; unchanged here |
| Reply line | 107.00 | 2,441.47 | Opt-in `reply-to <ts>` | IDs-only line | Minimal automatically selects Sinan's compact line |
| Peer wrapper | 19.92 | 454.56 | Keep; shorter prefix proposed | Opt-in shorter wrapper | No buffered block; immediate peer retains provenance |
| Peer items | Included in message | Included in message | Clips proposed | Keep | Recover buffered peers with channel read |
| Other, including help/batch directions | 138.98 | 3,171.23 | Anchor/tail cuts above | Shorter batch directions | No per-message how-to; each batch member keeps ids/text |

The minimum is channel name/id, sender name/id, new message ts/text, thread
root when replying, the short anchor, compact reply-to coordinates and one
context pointer per member. Immediate peer messages also retain the read-only,
not-addressed-to-you label: removing that would change their meaning. File
descriptions remain payload. Multi-message digests remain verbatim.

The loss is deliberate: earlier thread decisions and ambient peer reports are
not immediately present. Read the thread using the pointer; read the channel
without `--thread-ts` for peer posts. Reply syntax belongs in each agent's
prompt before enabling the flag. A cold parent cache retains only the thread
id; keeping a first-line quote cannot require a new history fetch per message.
Observed edits and deletions invalidate cached parent excerpts. Reaction
target lookups keep their original single-page scope; a minimal reaction
without a known thread root points to the channel rather than its event ts.
Minimal does not use or advance full mode's delivered-context cache.
Mention-only ownership recovery can still query Slack for an old thread not
in the own-thread registry; that routing fallback is preserved.

## Measured baseline and offline replay

The exact PR 59 measurement script and tests were imported from
`d6e924a95f797a0f6b904133d04474c6208b8cbc`, with source attribution in their
separate commit. No `compact_alerts.go` code was taken.

| Scenario | Bytes over seven days | Estimated tokens/day |
| --- | ---: | ---: |
| Recorded baseline, 575 blocks | 518,967 | 20,593.93 |
| Sinan's three commits, default full reply line | 480,219 | 19,056.31 |
| Sinan plus optional compact reply line | 433,644 | 17,208.10 |
| Minimal production-renderer replay | 321,589 | 12,761.47 |

**ANALYSIS:** minimal projects a 38.03% reduction against the measured baseline,
before the cost of on-demand reads. The replay has 657 message members in 575
blocks, including 73 batches. It passes parsed message units through the Go
production renderer, preserving the gc envelope/footer and using the compact
registered reply line. Historical reminders lack structured sender ids and
channel names: the projection supplies an 11-byte synthetic sender id and uses
the unresolved-channel fallback. Captured parent excerpts are retained.
Already-clipped source messages cannot be reconstructed. Resolved channel
names, cold parent caches, future traffic, reads and provider tokenization
change realized savings. This is not billing or deployment evidence.

`measure_injected_alerts.py` reproduced every baseline byte and all class
totals. More transcript files now pass the mtime prefilter (83 vs 55), but the
fixed timestamp window still produces exactly 575 blocks. Only numeric and
structural reports are committed under `assets/pr-evidence/gp-zhtd/`.

## Definitive content and delivery evidence

Read-only inventory found ownership in `own_threads.json`, outbound receipts
in `publish-receipts/`, channel activity in `inbound_liveness.json`, and rejected
envelopes in dead letters. `chat-ingress/` had no files. The city event journal
had 546 `extmsg.inbound` events with provider, channel, actor and target session/
agent, but no message ts or context window. None answered what a reminder
carried. `gc slack status` reports activity, not that missing metadata.

The new journal records each accepted forward on the same HTTP return path:
time, channel, thread, message ts, target handle, receipt-provided sessions,
adapter Text bytes, mode, quoted oldest/newest ts, collapsed count and skipped
watermark. Batch windows remain separate per member and survive the spool.
No message text or sender identity is written. Held receipts add a conclusion
record keyed by receipt id; repeated forwards are distinct attempts, not proof
of distinct session injections. Legacy gc responses without delivery evidence
are explicitly unconfirmed. A crash between gc's acceptance and local append
can leave a gap: this journal is diagnostic, not a distributed transaction.

Daily JSONL files live under `.gc/slack/inbound-journal/`; 16 MiB segments rotate
without discarding records, with `SLACK_INBOUND_JOURNAL_RETENTION_DAYS=7` by
default. Storage errors are logged and never trigger a duplicate delivery.
Slack through `gc slack read` is the definitive recoverable text source,
subject to its access and retention; the journal establishes what the adapter
forwarded and what gc vouched for. A pending/unsupported receipt cannot establish
that an agent received or remembered the text.

## Pack/core boundary and rollout

gc owns `<system-reminder>`, `New message in shared conversation slack/...`,
the outer author prefix and `Message id:` in
`/Users/tailor512/code/gascity/internal/api/handler_extmsg.go` (renderer at
line 564 in the referenced integration). Header/framing plus message-id
account for **144.57 bytes/block, 3,298.61 estimated tokens/day**. Including the
outer author yields **167.03 bytes/block, 3,811.11 tokens/day**. Citadel's proposed
header/id label cuts alone would save 1,048.61 tokens/day on this sample. That
is a separate gc-fork bead for the mayor; this branch changes none of gc.

Set the prompt's reply/read contracts first, then opt into
`SLACK_INJECTION_MODE=minimal` at the next approved adapter start. Unknown modes
fail startup. Full is unchanged apart from the long-thread pagination fix and
text-free diagnostics. The PR remains draft for the founder's first-head read;
the mayor owns the Fable review, merge word, PR 59 supersession and rollout.

# Slack injection: progressive disclosure, 2026-10-25

The PR 62 audit below records the original progressive-disclosure baseline,
built on Sinan's jg-fc1csv commits `84b8da44`, `910c232b`, `52cbd2f8`.
The **Peer parity** section records the follow-up on merged base `f507f718`.
It supersedes PR 62's peer omission and bot-only presentation rules. No live
adapter setting or agent prompt has been changed. Full is the default.

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
Page size is now 500 independently of that retained window, with a four-page
cap per context fetch. The root-plus-400-replies fixture takes one request,
down from 21 at the first head. A timeout, rate limit or unfinished pagination
retains the root and newest replies fetched so far, labels that window
incomplete, and leaves the delivery watermark unchanged for recovery later.
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

## PR 62 synthesis of the two audits (historical)

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

PR 62's minimum was channel name/id, sender name/id, new message ts/text,
thread root, short anchor, compact reply coordinates and a context pointer.
Its peer-specific tag and omission decisions are superseded below. File
descriptions remain payload. Multi-message digests remain verbatim.

Earlier thread decisions are read using the pointer. Reply syntax belongs in each agent's
prompt before enabling the flag. A cold parent cache retains only the thread
id; keeping a first-line quote cannot require a new history fetch per message.
Observed edits and deletions invalidate cached parent excerpts. Reaction
target lookups keep their original single-page scope; a minimal reaction
without a known thread root points to the channel rather than its event ts.
Minimal does not use or advance full mode's delivered-context cache.
PR 62 omitted ambient peers without a count and clipped failed-forward
recovery. Both decisions are replaced by peer parity below. Recovery remains
in memory; restarts are not durable recovery and sustained outages can grow it.
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
Round 2 changes only failed-forward recovery bytes in minimal mode. This
historical replay has no failed-forward state; its ordinary reminders and
321,589-byte projection are unchanged. Recovery traffic adds bytes when needed.

`measure_injected_alerts.py` reproduced every baseline byte and all class
totals. More transcript files now pass the mtime prefilter (83 vs 55), but the
fixed timestamp window still produces exactly 575 blocks. Only numeric and
structural reports are committed under `assets/pr-evidence/gp-zhtd/`.

## Peer parity (gp-j5p4)

The pre-design inventory and count comment precede implementation. Evidence
is under `assets/pr-evidence/gp-j5p4/`; no source transcript or live message text
is committed. The exact same window, **2026-10-14 23:00–2026-10-21 23:00 UTC**,
again yields **575 reminders / 518,967 bytes** from `measure_injected_alerts.py`.
The base Go renderer replay reproduces **321,589 minimal bytes**.

The captured sample has **110 bot-item copies in 79 buffered ride-along
blocks**, with zero immediate peer reminders. The adapter log records 150
buffered admissions in the same window (49 allowlisted, 101 other bots), zero
immediate deliveries and 41 budget deferrals. Log admission is not delivery.
The new journal starts after this window and cannot reconstruct it. The
measure-first artifact also records the latest seven-day log window and its
coverage limits.

Every allowlisted peer now arrives as an independent inbound in both modes,
with the registry sender label and whole text. The peer golden deliberately
changes to the ordinary human reminder shape: author, thread anchor, text and
reply coordinates, without the adapter's response instructions. The old
`wake` and `immediate_channels` keys still parse, avoiding a config migration;
startup logs once that every channel is immediate. Unlisted bots no longer
enter an ambient buffer, while history reads and quotes include all authors.
Current-reply/react/upload lookups also include admitted peer inbounds, with
the same latest-message rule for event selection and transcript anchoring.
This removes the leftover bot skip that could pair a new peer conversation
with an older human thread. Synthetic reaction notifications stay excluded.
The measure-first inventory recorded the former human-only anchor policy;
review showed it could not coexist with the new ordinary peer event shape.

Full context gives humans, own bots, peers and other bots the same whole
quotes, chronological order, delivered collapse and counted window omission.
There is no 280-byte clip, 1,400-byte bot allowance or bot-first shedding.
The 3,500-byte budget applies to optional context/help; new message text and
attachments stay whole. Minimal mode still reads previous context on demand.
Both read modes keep the newest N messages, across every author; thread reads
scan to the final cursor instead of returning the oldest 50.

Failed forwards alone enter the FIFO. Recovery preserves whole text, uses the
same budget in both modes, restores failures ahead of deferred entries and
reports `[N omitted — awaiting delivery]` whenever entries remain. There is
no 20-entry eviction or 2,000-byte clip. An item too large to fit remains
queued with a count until there is enough budget, rather than being counted
as delivered after clipping. This queue is still in memory.
Recovery retains attachments and trusted file paths. A refused rider cannot
charge an unrelated successor's rejection ladder: riders are deferred and the
successor retries alone with a count. Rejected attachments retain a path and
withholding notice; held-loss dead letters retain every included peer.

### Cost of a peer post

The replay promotes each of the 110 captured buffered copies to an admitted
peer arrival and renders it with production Go functions. Figures include
the gc envelope and reply line on each new arrival. Before-full amortizes the
shared wrapper over the captured posts; before-minimal omitted those posts.
Estimated tokens use UTF-8 bytes / 3.6, as in the original audit.

| Average per captured peer copy | Before bytes | After bytes | Before estimated tokens | After estimated tokens |
| --- | ---: | ---: | ---: | ---: |
| Full: buffered rider → own reminder | 852.81 | 968.54 | 236.89 | 269.04 |
| Minimal: omitted rider → own reminder | 0 | 1,040.76 | 0 | 289.10 |

| Seven-day scenario | Before bytes | After bytes | Estimated tokens/day delta |
| --- | ---: | ---: | ---: |
| Full arrival-cost projection | 480,219 | 492,949 | +505.16 |
| Minimal production-renderer replay | 321,589 | 435,938 | +4,537.66 |

The minimal replay now contains **685 reminders / 767 members**, including
the original 73 batches. Its total is **17,299.13 estimated tokens/day**.
The 114,484 bytes for new peer arrivals are offset by 135 bytes of retired
bot-tag overhead elsewhere in the sample. The full projection substitutes
106,539 arrival bytes for 93,809 buffered-block bytes, including separators.

Rows from the original audit that move:

| Class | Before | After and reason |
| --- | --- | --- |
| Peer wrapper | 11,455 bytes | 0; wrapper and footer retired |
| Peer items / new message | Buffered in full; omitted in minimal | Every admitted peer is a separate whole-text message |
| Header, author, message id and reply line | Shared the human wake | Each new peer reminder pays its own envelope and reply coordinates |
| Thread quotes | Bot clip/allowance and bot-first shedding | Same context rule for every author; historical missing originals cannot be reconstructed |

**Projection limits:** these are delivered copies, not unique Slack posts or
a future traffic forecast. Only 18 copies match labels in today's registry;
historical identities and registry state are unavailable, so the 110-copy
scenario explicitly models all captured items as admitted peers. It does not
silently count the removal of unlisted traffic as a saving. Captured clipped
text cannot be expanded, and full-mode context at each peer's original arrival
is unknown. The full number isolates arrival overhead and does not estimate
the added own-bot/longer thread quotes. Reads, resolved names, unobserved posts,
recovery and provider tokenization affect realized cost. No adapter was
restarted or switched to minimal for this measurement.

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
fail startup. Peer parity applies in full and minimal modes. The PR remains
draft for the founder's first-head read;
the mayor owns the Fable review, merge word, PR 59 supersession and rollout.

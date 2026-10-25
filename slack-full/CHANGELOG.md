# Changelog

All notable changes to slack-full (formerly slack-pack) are documented in
this file.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- **Progressive-disclosure reminders** (`SLACK_INJECTION_MODE=full|minimal`,
  default `full`, gp-zhtd), with compact reply coordinates and on-demand thread
  reads. Minimal mode skips context fetches and omits repeated thread/peer
  context and reply how-to. Install the agent prompt contract before opting in.
- **Text-free inbound delivery journal**, daily JSONL segments with configurable
  retention, selected context-window metadata and gc receipt/session evidence.

- **`SLACK_REPLY_INSTRUCTIONS=compact`** (jg-fc1csv) registers an ids-only
  per-message reply line, `reply-to <ts>`, instead of the full
  `Reply: gc slack reply-current --conversation-id … --reply-to … --body-file
  <file>` command (~22 fewer tokens on every inbound reminder). Opt-in, default
  `full`: the command then has to live in each bound agent's prompt, because
  the once-per-channel how-to is per adapter lifetime, not per agent session.
  See SETUP.md.

### Changed

- **Thread context pagination** now preserves the root and newest replies
  across cursor pages, instead of losing the newest end of long threads.
  Current/later messages are excluded before selecting an inbound's window.
- **Thread replies no longer carry the `[slack thread_ts=<root ts>]` marker
  line** (jg-fc1csv). The protected `[thread reply …]` anchor already names
  the thread ts at the head of every thread-reply delivery and gc's
  `Message id: … (in thread …)` line repeats it at the tail, so the marker
  was a third copy (~13 tokens per thread reply). Nothing parses it: the
  reply/react/upload helpers read the transcript's structured fields.
- **Shorter thread-reply anchor and collapsed-priors line** (jg-fc1csv). The
  anchor reads `[reply in thread <ts>; parent @<name>: "…"]` instead of
  `[thread reply — reply with --thread-ts <ts>; …]` — `--thread-ts` is not a
  `reply-current` flag, and the per-message reply line already spells the
  command. Already-delivered priors collapse to `Thread context: N earlier
  messages already delivered.` without the `(newest ts …) — not re-quoted`
  tail.

### Fixed

- Redact secret-shaped inbound Slack text before durable admission, gc journal
  and mail delivery, shutdown spooling, and dead letters (gp-pnfn). The pack's
  tail helper imports the same rules and redacts before truncating output.
  JWT matching requires an `eyJ` header prefix to preserve ordinary dotted
  identifiers; pre-upgrade held replay text is scrubbed on state load.

- **Accepted text posts no longer fail when readback is unconfirmed** (gp-yj6n).
  A Slack message timestamp now preserves `delivered=true` with no publish
  failure kind. Readback absence, text mismatch, transport failure, and auth
  failures remain visible in receipt metadata and the durable evidence store.
  Keyed retries recheck the accepted timestamp without reposting; confirmation
  bookkeeping remains once-only. The three text publish/reply CLIs include
  metadata in terse JSON, exit zero on a timestamp (including legacy receipts),
  and report unconfirmed reads on stderr. See [delivery evidence](docs/delivery-evidence.md).
  The gp-obim normalizer already handles the incident's two mailto wrappers;
  this change preserves that comparator and adds recorded-incident coverage.

- **Keyed outbound messages no longer carry a visible reference footer**
  (gp-obim). Supersedes the entry "Outbound publishes carrying an idempotency
  key are stamped with a low-visibility reference marker" below. Message text
  is sent unchanged after the existing alias rewrite, and `MaxMessageLength`
  again advertises the full 40,000-character ceiling. Every publish readback
  uses exact timestamp and canonicalized text equality, including thread
  replies via `conversations.replies`. Bare-domain auto-links (for example,
  `hou.vc` stored by Slack as `<http://hou.vc|hou.vc>`) confirm without reposting.
  URL tokens use one boundary rule for full and implicit links: enclosing
  punctuation, formatting runs (including `**` and `__`), sentence punctuation
  and trailing Unicode emoji leave the URL literal and the suffix as prose.
  Host-adjacent emoji aliases normalize consistently; internal path/query
  identifiers and embedded returned link segments remain literal.
  Delivery evidence now lives in the adapter's private
  `<GC_CITY_PATH>/.gc/slack/publish-receipts/` store (fallback:
  `/tmp/gc-slack-adapter/publish-receipts/`), keyed by the full SHA-256 digest
  of the caller's idempotency key. The adapter writes acceptance immediately
  after Slack returns a message ts, then updates the readback state atomically.
  `GET /publish-receipt?idempotency_key=<URL-encoded key>` on the existing
  internal server returns the same flat receipt fields as `publish-to-channel`
  (`delivered`, `conversation_id`, `message_id`, optional `thread_ts`,
  `reply_to_message_id`, and `failure_kind`), plus `posted_text_sha256`,
  `posted_at`, and `readback`. The digest covers the exact alias-rewritten
  bytes posted. Readback is `pending`, `confirmed`, `readback_unconfirmed`,
  `readback_unavailable`, or `readback_auth`; pending proves acceptance only.
  Receipts survive restarts and expire after 30 days from posting, swept at
  startup and hourly. This later-reader audit window is longer than the
  company inbound receipt store's seven-day retry window. Corrupt receipts
  are removed on lookup or by the sweep with one log line; the first corrupt
  lookup returns 404 immediately. I/O failures remain operational errors.
  A reused key records the newest accepted post. A 404
  means **unknown**, never undelivered; unkeyed posts, files, and deliveries
  before this change have no stored evidence. Storage errors are 500 on lookup
  and `metadata.receipt_store=unavailable` on publish. This store records past
  delivery observations and does not extend the in-memory dedup TTL or check
  for later message deletion. The normative contract is in
  `adapter/publish_receipts.go`; no additional Slack scopes are needed.

### Added

- **Mention-only room bindings** (jg-vobf70, Afik 2026-09-10 for the US
  fleet channel): `gc slack bind-room <C…> <session> --mentions-only`
  registers the session with the adapter (`POST /mention-only`) instead
  of as a gc group participant. The adapter injects a `Slack
  mention-only room delivery` reminder into the session only when a
  message @mentions the bot user (or arrives as `app_mention`),
  addresses the session's handle (`@handle:` prefix, User Group
  mention, thread-sticky handle), or replies in a thread the session
  posted in (new own-thread registry fed by `/publish` and
  `/publish-file`, with a scan of the thread for the bot's own user as
  the pre-registry fallback). Everything else in the room is dropped
  for that session — no wake — while ambient participants, the channel
  copy to gc and the `@handle:` alias dispatcher are unchanged (an
  alias injection into the same session suppresses the mention-only
  copy; Slack's message/app_mention twin pair collapses to one
  injection under a per-(session, channel, ts) claim; a failed
  injection releases its claim and marks the message ⚠️). New
  endpoints: `POST`/`DELETE`/`GET /mention-only`,
  `GET /mention-only/deliveries?session_id=`; new env
  `SLACK_MENTION_ONLY_BINDINGS_FILE`, `SLACK_OWN_THREADS_FILE`.
  The reminder prescribes the bindingless `publish-to-channel
  --thread-ts` reply. Pack side: `reply-current --thread-current`/`--turn-ts`, `react`, and
  `upload --thread-current` resolve mention-only deliveries through the
  adapter's delivery log and publish via the adapter (the session has
  no gc binding there); `gc slack status` lists mention-only bindings
  from the pack config and the registry file; the bind sends a
  mention-only variant of the reply-protocol nudge. Round 4 (citadel
  gate MAJOR): `reply-current` resolves mention-only deliveries BEFORE
  the automatic company-room/DM dispatch — a delivery newer than the
  session's company current-turn pointer (or the one `--turn-ts` names)
  is answered via the adapter instead of being diverted into the older
  company conversation; `--turn-ref` / `--origin-ts` / `--kind
  room|dm|mpim` still pin the company turn. Round 5 (citadel gate r3
  MAJOR, `main.go:4058`): imported company rooms no longer bypass
  mention-only delivery — the company gateway, which consumes those
  rooms' events before the legacy dispatcher, now runs the
  mention-only lane itself on first admission for every registered
  session outside company membership (bot @mention, `@handle:`
  prefix, own-thread follow-ups; human-authored messages only; a
  binding for a company member of the room is skipped, so no double
  delivery). Gate r2c MINOR: `bind-room --mentions-only` for a session
  the pack record lists as ambient now names `--replace-ambient`, which
  drops the stale ambient entry (participant / binding owner) once the
  gc-side membership is gone; a binding gc still reports active stays
  refused. Round-4 P2s: re-binding a session under its other
  identifier (name ↔ resolved id) replaces the registry record instead
  of appending a second binding (double injection); an ambient
  re-bind of a handle to a new session drops the replaced session from
  the pack record (gc upserts by handle). Round-5 codex: a company-room
  injection gc rejects is retried in place on a bounded backoff, and
  the app_mention twin / a Slack redelivery retake the released claim
  (the receipt is already acked, so nothing upstream retried it); a
  company binding targeting another city no longer shadows a local
  mention-only session of the same name; `reply-current` treats gc's
  200 + `FailureKind=auth` receipt like an HTTP refusal when choosing
  the mention-only adapter route. Round 6 (citadel gate r4 MAJOR,
  `slack_chat_bind_room.py:483`): the bind conflict checks compare a
  session's full identity set — gc id + alias + session name — so an
  ambient participant recorded under its session name is no longer
  invisible to `--mentions-only` by id or alias (the bind used to
  succeed on top of the ambient membership: every-message wakes PLUS
  mention injections); `--replace-ambient`, the mirror check for an
  ambient bind and the live gc-binding probe use the same set. Gate r4
  MINOR: `gc slack status` prefers the registry's values (what the
  adapter enforces) over a matching pack-config row. Fable read r1:
  the gateway's copy of the config now carries the shutdown drain flag
  (it was assigned after the copy, so the lane's stop-retrying branch
  was dead) and shutdown joins the lane; the lane records the sessions
  it reached on the durable ingress receipt
  (`mention_only_delivered`), so a Slack redelivery after a restart or
  past the 10-minute in-memory claim does not wake the session twice
  (the write follows the injection — a crash in between can still
  deliver twice, never zero); the ⚠️ posted on a failed injection is
  removed when a later attempt reaches every session it stood for;
  with `SLACK_APP_ID` set, only the switchboard app's copy counts as
  "the bot was mentioned" (a persona app's `app_mention` or bot user
  id does not), and `SLACK_SWITCHBOARD_BOT_USER_ID` names the bot user
  when set. Round-6 codex: injections still failed when a lane run ends
  (retry ladder exhausted, or cut short by shutdown) are left on the
  receipt (`mention_only_pending`) and replayed once after the next
  startup recovery — the event was acked at admission, so nothing else
  would retry them; the targets are written as pending BEFORE the first
  POST, so a process that exits mid-run leaves them for the replay; gc's
  asynchronous 202 acceptance is awaited to its terminal result on the
  event stream (a `request.failed` is a failed injection, not a
  delivery; an interrupted stream re-confirms the same request rather
  than re-posting); a copy skipped on its twin's committed claim
  settles the delivery on the receipt (the twin may have outrun the
  receipt); only injections a run saw gc deliver are recorded as
  delivered (the delivery log is provisional); a mention-only bind
  removes the session's registration held under another identifier
  (double injection) and registers the session's further gc identifiers
  as `aliases`, which the adapter matches everywhere it compares a
  binding to a session (company-member exclusion, own-thread posts,
  upsert, delete, delivery log); the ambient participant merge folds
  handle case like gc does; `gc slack status` labels a pack-config row the
  registry no longer holds as stale. Round 7 (citadel gate r5): the
  legacy (non-company) lane awaits gc's asynchronous 202 to its terminal
  result too, through the same hook as the company lane — a 202 whose
  request then failed used to commit the delivery claim, the Slack twin
  was skipped on it and the session received nothing; a failed legacy
  injection is retried in place on the same bounded backoff (Slack was
  already acked, and an own-thread follow-up has no `app_mention` twin;
  this lane has no receipt, so retries cut short by shutdown are not
  replayed); `upload --thread-current` on a mention-only delivery that
  was a thread reply anchors at the thread root, not the reply's ts;
  the legacy lane runs beside the channel copy and holds it at most
  10 s (gc concludes an injection at the session's next idle boundary);
  a delivery-log record stays `provisional` until gc concludes it
  delivered, and the pack scripts never pick a provisional record as the
  session's latest inbound or over a company pointer (an explicit
  `--turn-ts` still resolves it); the receipt's delivered marker is
  matched across a binding's identifiers; the legacy lane's reminder
  names files the adapter could not download; a room's mention-only
  recipients are injected concurrently, so one busy session does not
  hold the others' copies; `upload --thread-current` refuses to pick a
  mention-only room when the session's company current turn is at least
  as new as that delivery (pass `--conversation-id` + `--thread-ts`).
  Round 8 (citadel gates r6/r7 + read r3): `upload --thread-current`
  compares the company pointer against the exact confirmed delivery it
  selected — a still-provisional newer record of the same room used to
  outvote a newer company DM and send the file into the old public
  thread; selection and comparison are now one shared helper for
  `reply-current` and `upload` (`select_mention_only_delivery`,
  `mention_only_delivery_supersedes`), and a provisional record resolves
  only when the session has no confirmed record at all, still below any
  gc inbound or company pointer (the mark outlives gc's delivery by the
  event-stream latency); the company lane resolves Slack User Group
  mentions — labeled through the handle-alias registry, unlabeled
  through subteam-aliases.json — with the legacy dispatcher's own
  address resolution (`resolveAddressTarget`), so a mapped subteam
  mention reaches a mention-only outsider of a company room; the lane's
  replay intent (`mention_only_lane`) is written in the receipt's
  admission write, before the Slack ack, so a process that exits before
  the lane records its selection has the whole lane re-run at the next
  startup; a failed legacy injection records its warning before it
  releases the claim (no false permanent ⚠️ when a twin delivers first);
  a pending entry is cleared across a binding's identifiers; the
  reminder says the `--thread-current` shortcuts resolve a delivery once
  gc has confirmed it. KNOWN LIMIT (documented in
  `company_mention_only.go`, not changed): gc caps a `session.message` at
  four minutes and emits `request.failed` code `timeout`, which the
  adapter reads as definitive — a session busy longer than that looks
  undelivered and is re-posted up to three more times; whether gc then
  queues or drops the cancelled copies is unverified (owner test before
  a city with long turns adopts the pack); the closing shape is to treat
  the timeout code as pending and re-confirm the same request.
  Round 9 (citadel gate r8 + read r4): in the multi-app layout
  (`SLACK_APP_ID` set, persona apps subscribed to the room's message
  events) with `SLACK_SWITCHBOARD_BOT_USER_ID` unset, a persona app's
  copy cannot recognize a switchboard @mention; it selected nobody and
  settled the admission intent it shares with the switchboard's copy,
  so an exit after the switchboard copy's ack and before its pending
  write lost the mention. The intent now carries the delivering app
  (`mention_only_lane.app_id`); a copy that cannot recognize the
  mention never settles it (what it does select is still delivered);
  the switchboard's duplicate copy takes over an intent a persona copy
  wrote, before its ack (503 when that write fails, so Slack
  redelivers); the startup replay runs as the intent's app. The layout
  is therefore safe without the variable, but set it: without it, a
  room the switchboard app delivers no copy for keeps every human
  message's intent and re-selects it at each startup until the
  replay's 24-hour bound. Same-second confirmed delivery records order
  by Slack ts. Costs to know: every human message in a room with
  outsider bindings takes one extra generation-checked receipt write
  (the settle); `mention_only_lane` is a pointer with `omitempty`, so a
  pre-round-8 receipt decodes as nil (no replay intent — such receipts
  predate the lane's replay) and a round-8 one without `app_id` replays
  as the app that created the receipt.

### Changed

- Token-efficiency batch (gp-9e7, Afik-approved 1787421193 with items
  1–4 in scope per follow-up ruling 1787421279):
  1. **Reaction-event buffering** — reaction notifications (gp-by3)
     never wake a session solo anymore, in rooms or DMs, coalescing
     enabled or not: they buffer in a no-wake side lane of the
     coalescer and ride the channel's next real delivery (batch flush,
     urgent/DM flush-ahead, or shutdown drain). Exception: a reaction
     ON one of the adapter's own outbound posts may join an
     ALREADY-armed coalesce window (founder acks land with the batch);
     it still never arms one. Overflow past 100 buffered reactions
     delivers instead of evicting — nothing is ever dropped.
  2. **Cross-channel coalescing** — a firing flush timer now sweeps
     every other buffered non-digest channel, so ONE idle wake
     delivers all channels' window traffic as aligned back-to-back
     per-channel batches (each formatted byte-identical to before;
     order preserved within channel). Digest-mode channels keep their
     operator-configured interval and are never swept; channels
     holding only buffered reactions are never swept either (that
     would be a solo reaction wake).
  3. **Bot-post buffering generalized** — the gp-kop peer-bot buffer
     now admits ALL bot-authored posts (same fail-closed bots.info
     self-guard; unknown bots labeled from bot_profile/bots.info).
     Unknown bots can never wake a session; the peer_bots.json
     allowlist's role is granting wakes — per-entry `"wake": true` or
     the existing `immediate_channels` — and none are granted today.
  4. **Terse send receipts** — `reply-current`, `publish`,
     `publish-to-channel`, and `upload` no longer echo the full result
     envelope (gc transcript entry, outbound text included) into the
     sending session's context. The default receipt is delivered flag
     + message_id/file_id + conversation_id + thread_ts when threaded;
     delivered=false receipts keep failure_kind and the error message.
     `--verbose` restores the full envelope. Exit-code semantics are
     unchanged.

### Fixed

- **A HOME-less rebuild no longer uses a shared, guessable build cache.**
  When HOME, XDG_CACHE_HOME and GOCACHE are unset, `adapter/run.sh` used to
  build with `$TMPDIR/gc-slack-adapter-gocache` and
  `$TMPDIR/gc-slack-adapter-gopath`. Another local user could create those
  directories first and plant cache entries that end up in the adapter
  binary. The script now uses per-user `0700` directories named with the
  uid, and refuses to build if one is a symlink, is not a directory, or is
  owned by another user. slack-mini and slack-channel got the same fix in
  #492.

- `_request` in `scripts/slack_intake_common.py` let two failure shapes
  escape as non-`GCAPIError` exceptions. A peer that answers but breaks the
  protocol (`IncompleteRead`, `BadStatusLine`) raises
  `http.client.HTTPException`, which is neither an `OSError` nor a urllib
  error, and an `HTTPError` whose error body cannot be read raised from
  inside its own `except` clause. Both now become `GCAPIError`; the
  `HTTPError` path keeps the status code when the body is unreadable.
  (`dr-3lhmr`)

- `gc slack status` reported a section it could not read as
  `(none registered)` and exited 0, which is exactly what it prints for a
  section that is genuinely empty. A reader could not tell "I looked and
  there is nothing" from "I could not look". Each of the four reads
  (adapters, inbound events, outbound events, bindings) now records why it
  failed; the report marks those `(UNREADABLE: <reason>)` and the JSON
  output carries them under a top-level `unreadable` key. **The exit code
  changes: 2 when any section was unreadable, 0 when every section was
  read.** The sections that were read are still printed in both cases.
  (`dr-3lhmr`)

- `manifest/app.json` and `manifest/agent-app.json` no longer ship
  `features.slash_commands: []`, which Slack's manifest validator rejects
  at import, and `manifest/app.json` now declares the `app_mentions:read`
  bot scope its `app_mention` subscription requires (#63).
- `gc slack reply-current` now inherits the thread from the latest
  inbound (gp-i62): a thread-reply inbound's transcript entry carries
  the Slack `thread_ts` in `ReplyToMessageID`, and the reply anchors
  there by default — including when `--conversation-id` names the same
  conversation explicitly, which previously always posted at channel
  level (live burns 2026-08-09, #gastown + #fundraising). An unthreaded
  inbound keeps the channel-level reply; an explicit target naming a
  *different* conversation never borrows the inbound's thread anchor;
  a failed lookup degrades to channel level with a stderr warning
  (`--via adapter` must survive a gc outage). New `--no-thread` flag
  forces a channel-level post. `--thread-current` now anchors at the
  thread ROOT when the latest inbound was itself a thread reply —
  Slack threads hang off the parent ts, so anchoring at the child
  stranded the reply. Retires the fleet-memory workaround of routing
  threaded replies through `publish-to-channel --thread-ts`.
  `reply-current` now also reports the anchor it used —
  `reply_to_message_id` in the result JSON, plus an `inheriting thread
  <ts> from inbound <mid>` line on stderr when inheritance fires — so a
  reply that lands in an unexpected thread can be traced to the inbound
  that donated the anchor. The inherited anchor is the conversation's
  newest inbound, not provably the message being answered (nothing on
  the wire identifies which inbound woke the session); the limitation
  and its `--reply-to` escape hatch are documented in
  `commands/reply-current/help.md`.
- `gc slack upload --thread-current` anchors at the thread ROOT too,
  matching the parity its help text promises with `gc slack
  reply-current`. It consumed a message-id-only lookup that dropped the
  thread root, so a file uploaded in answer to a thread reply anchored
  at the child ts and stranded outside the thread.
- A *wedged* gc (connection accepted, then stalled) no longer crashes
  callers whose degrade guards catch `GCAPIError`: the HTTP helper
  wraps `TimeoutError` — raised bare out of `resp.read()`, an `OSError`
  rather than a `URLError` — so a stalled lookup degrades like a
  refused one instead of killing the reply with a traceback. The wrap
  covers the stall's siblings too: a gc killed mid-response sends an
  RST and `resp.read()` raises `ConnectionResetError`, so every
  transport `OSError` now reaches callers in that same currency.

- **A 4xx-refused inbound is never re-posted as-is; transient failures
  back off** (gp-sgu7; citadel C0AP0KV9S9E 2026-09-08 00:14Z →
  2026-09-11, C0BKF28CYUE 2026-08-26 → 08-27). The live adapter posted
  one batch gc refused with 422 (`expected required property mime_type
  to be present` on a Slack voice memo) every 8 s for three days —
  34,000+ identical failures — while every later message in the channel
  joined the stuck batch (`batch=1` → `batch=44`) and none reached the
  session. That binary predated gp-xnc (#21: mime_type always sent,
  three identical retries then dead-letter); this tightens the contract
  gp-xnc left:
  - **The rejection ladder is one function** (`nextRejectionStep`,
    `inbound_dead_letter.go`; `charge` is its only consumer). A payload
    refusal (400/413/415/422) with attachments is re-posted ONCE
    *without* them — the text keeps the founder's words and names the
    downloaded files' local paths, so an attachment gc will not
    validate costs the attachment, never the message. A refusal with
    nothing left to strip — the second one, a plain text, a reaction, a
    deletion notice — dead-letters at the first refusal. The same bytes
    are never posted twice (one deliberate exception: a dead-letter
    write that fails keeps the entry for the next window rather than
    losing an acked message). The unvouched ladder (gp-32q, bounded by
    `maxCoalesceDeliveryAttempts`) is unchanged: that payload was
    accepted, so a re-post can succeed.
  - **Transient failures (network, 5xx, 429) back off per channel**:
    the retry cadence starts at the channel's window and doubles per
    consecutive failure to a 5-minute cap (`maxTransientRetryDelay`),
    replacing any plain-window timer a mid-flight enqueue armed; a
    channel waiting out its backoff is not swept into another
    channel's flush; the run resets on the next successful delivery. A
    digest interval longer than the cap stays the operator's cadence.
    Retry-forever durability is unchanged — only the cadence.
  - One log line per state change: `refused … retrying ONCE without
    them`, `dead-lettered after N`, `… — transient failure #N, retry in
    D`.
  - **One scheduler, one deadline writer** (codex gate r8). Eight gate
    rounds each found another path arming its own timer around the
    backoff deadline (enqueue, the over-cap poll, restore, a timer
    firing under the deadline, the reconcile, the urgent flush-ahead's
    twin withhold, the reaction overflow). Now `scheduleLocked` is the
    only place a channel's timer is armed, moved or disarmed — from
    the channel's state alone: nothing timer-worthy (no real message,
    no reaction overflow) means no timer; otherwise the earlier of the
    current target and the caller's want, never before the deadline —
    and `noteTransientFailure` the only writer of the deadline. A
    restore re-queues and asks; it decides nothing about cadence, so a
    withheld twin handed back after a failed mention keeps the deadline
    where it was, an overflowed reaction lane during a backoff flushes
    AT the deadline instead of never, and a buffer emptied under a
    still-armed timer loses the timer instead of POSTing reactions
    alone. A recovery pulls the buffered retry in to the window. With
    no timer and no want the retry IS the deadline when one is ahead,
    and a SIGHUP reconcile keeps an established target that is nearer
    than the new policy's window (codex gate r9). `TestScheduleTable`
    is the cadence contract.
  - A take collapses same-ts copies to the one furthest along the
    rejection ladder before segmenting, so a duplicate admitted while
    the original waited out a backoff flagged for isolation cannot
    post the refused bytes again after the original is dead-lettered;
    a parked dead-letter write carries a deletion that landed while
    the write was owed (codex gate r9).
  - **The ladder's verdict is about the message, not the copy** (codex
    gate r10). `charge()` records its verdict per `(channel, ts)`: a
    copy of a dead-lettered ts admitted later (or handed back by a
    failed urgent post) is dropped, a copy admitted while the stripped
    retry is owed enters AS that retry, buffered copies leave when the
    verdict lands, and an urgent twin of a message already on the
    ladder is not posted (`onLadder`; the stripped retry or the
    dead-letter file is its delivery) — the bot-mention pair, a
    redelivery, and a duplicate enqueued while the original's POST is
    in flight can no longer re-post bytes gc refused.
  - Tests: `inbound_rejection_ladder_test.go` (the ladder table, the
    withholding notice, the incident end to end, dead-letter after the
    stripped retry with later messages delivering, immediate
    dead-letter with nothing to strip, the backoff table, real-timer
    backoff + reset, sweep skip). Five gp-xnc tests that pinned "three
    identical retries" were rewritten to the new ladder; one
    (`…DeadLettersAfterMaxAttempts`) was removed as superseded.
  - gascity half (separate PR): `extmsg.ExternalAttachment.mime_type`
    becomes optional so one unknown file type can never refuse a
    message again. This adapter already always sends it (gp-xnc).


- **A delivery the receipt gate HELD is now followed up, and a lost one
  is recovered or dead-lettered — never a silent `HELD` terminal state**
  (gp-3yg, P0 per Afik 2026-08-29 01:39Z). A hold concludes the same-ts
  claim — that ruling stands, a re-post into a busy session duplicates
  the message — but until now a hold was also the END of the story: if
  gc's background send never landed, the adapter held no second copy
  and the only trace was the `HELD` log line. Taylor's C0B0Y964Q1Z
  message of 2026-08-28 (ts 1787901226) was lost exactly this way and
  found 19 hours later.
  - gc now records every inbound fan-out's conclusion by receipt id and
    answers `GET /v0/city/{city}/extmsg/inbound/receipts/{receipt_id}`
    with `state: pending | concluded | unknown` (gascity gp-3yg, fork
    PR). After a hold the adapter polls it (5s cadence, 10 min horizon)
    from all three claim-holding legs — urgent channel copy, coalesced
    batch, address-by-handle injection.
  - `concluded` + vouched/no_route → `receipt follow-up: RESOLVED`; the
    hold was right, nothing else happens.
  - `concluded` + failed/partial/short, or `unknown` (gc restarted and
    the fan-out died with it) → `receipt follow-up: LOST`: ONE re-post
    of the identical envelope (dedup-keyed — gc dedups the transcript by
    provider message id), `RECOVERED` only when that copy is vouched
    for or positively landed (complete byte counts, just the submit
    unconfirmed). A recovery copy that is itself held is followed once more; one
    gc reports as reaching nobody, accepts without a receipt, or does
    not vouch for leaves the loss STANDING: written to the channel's
    dead-letter file and logged at `LOSS` grade. A dead-letter record's
    reason says which claim it makes — "lost" (the agent did not get
    this; re-post from the file) or "unknown state" (a human must check
    the transcript first) — and the `LOSS` log line is the alarm that a
    record was written (or that the write failed), either way. Every poll answer is
    checked to be about the receipt that was asked for (a stale or
    mis-routed answer is not an answer), and a recovery re-sends the
    exact bytes that were held — the alias leg renders its reminder
    once and re-sends it, rather than re-rendering.
  - Still `pending` at the horizon, or at shutdown (one final poll
    after the drain flips; sleeping loops are woken and in-flight polls
    cancelled, and a hold noted after the drain began is recorded
    synchronously): not a loss, so NOT re-posted — but recorded in the
    dead-letter file with an explicit "state unknown, verify by hand"
    reason and a `STUCK` log line, never silence. Shutdown waits up to
    25s for outstanding follow-ups to write their records.
  - A gc that never offers a usable receipt status for the whole
    window (a 404 from a gc without the endpoint) fails OPEN to the
    plain hold with one `UNVERIFIED` line, so this pack is safe to pin
    ahead of the gc rebuild and arms itself once one is running; a
    transient 404/5xx (gc restarting, a city's server being replaced)
    is retried until the deadline, not taken as the answer — and a
    hold gc did answer for before going silent is recorded as
    unknown-state at the deadline, never failed open (the endpoint
    exists; the silence may be a restart that lost the fan-out). A lost
    address-by-handle injection is recorded with the aliased session,
    the handle and the exact rendered reminder (`alias_session_id`,
    `alias_handle`, `alias_body`), so a hand recovery re-injects the
    right bytes into the right session rather than the channel envelope.
  - The busy-probe shape is resolved by per-member landing evidence.
    A complete bracketed paste whose submit merely went unobserved was
    reported `failed 0/N`, which the gate correctly read as "a retry is
    clean" — on 2026-08-28 08:24Z the adapter re-posted a 1215-byte
    message six times and then dead-lettered it while the session had
    answered two seconds earlier. gc core fixes the classification in
    bead gp-2io: that member is `pending` WITH its real byte counts
    (typed delivery evidence; "landed, submit unconfirmed" is "not
    yet", never "failed"). The follow-up reads that shape on positive
    per-member proof: a CONCLUDED receipt whose members all either
    took the payload or had it pasted whole with only the submit
    unconfirmed is `RESOLVED` when the summary is `pending` or
    `partial` — never re-posted (the payload is in every pane; a mixed
    room folds delivered+pending to `partial`, a status the verdict
    would otherwise read as a clean retry) and
    never dead-lettered (a "verify by hand" record for every fast turn
    the probe missed would be the incident's false-alarm noise,
    reborn). A concluded-but-pending receipt WITHOUT landing evidence
    still goes to the deadline and is recorded as unknown-state.
- **A same-ts twin can no longer skip a copy the session never got**
  (gp-32q, pc_2e2378b9918e; 2026-08-27 22:57 CT and 2026-08-28 04:08
  CT founder-inbound incidents, both on the PR #23 build). A
  bot-mention arrives as a twin pair (`message` + `app_mention`, one
  ts, distinct event ids); the winning twin concluded its
  `(channel, ts)` claim on gc returning 2xx, and the parked twin then
  logged `channel copy already delivered by same-ts twin — skipping
  channel post`. But 2xx cannot see the last hop: gc's
  `/extmsg/inbound` handler notifies members in the BACKGROUND
  (`runBackground` -> `extmsgNotifyInboundMembers`) and answers before
  the session is touched, so an inbound gc accepted whole still
  reached the session as a fragment — 867 chars posted, only the reply
  boilerplate tail delivered — and the one copy that could have
  recovered it was the copy that skipped.

  The claim now concludes on a DELIVERY RECEIPT: gc vouching that the
  complete payload reached the session (the gascity side is gp-2rq).
  Applies to all three claim-holding legs — the urgent channel copy,
  the coalesced batch, and the address-by-handle session injection:
  - **Vouched** (or `no_route`, where a twin's re-post would resolve
    the same empty route): claim committed, twin skips — today's
    behavior.
  - **Not vouched** — an explicitly negative status (`partial`,
    `failed`), a member gc names as not delivered, or a `delivered`
    whose own `delivered_bytes` fall short of `expected_bytes`, checked
    per member as well as on the summary (the summary counts are SUMS
    across the notify fan-out, so a member that got half its payload
    can hide inside a total that adds up; and only when both counts
    arrive as numbers in the SAME unit — gc measures bytes at the tmux
    paste buffer, and comparing those against a rune count on an
    emoji-carrying Slack message is a different number, not a rounding
    error): the payload is
    re-posted ONCE in place while the claim is still held, so no twin
    can duplicate the attempt. If that still does not vouch, the
    urgent and injection legs release the claim and take the existing
    failed-delivery path, which is what lets the parked twin re-post;
    gc dedups a re-posted transcript record by `provider_message_id`,
    so the cost is a duplicate notification at worst. The coalesced
    leg fails into the same recovery machinery, with the unvouched
    error charged like a gp-xnc payload rejection: bounded retries,
    then the dead-letter file — never restored uncharged, which a
    never-vouching receipt would turn into an unbounded re-notify
    loop, and never silently retired, which would discard the only
    copy an ordinary one-event room message has. Every one of these is
    logged at `UNDELIVERED` grade with the receipt id.
  - **`pending` HOLDS — it never re-posts** (mayor ruling,
    2026-08-28). gc waits for a session to reach an idle boundary
    before pasting (`NudgeIdleTimeout`, 30s on this city), longer than
    any HTTP budget the adapter can hold, so `pending` is the NORMAL
    receipt for a BUSY session — and busy sessions are exactly the
    population this bug affects, since the truncation only manifests on
    a busy TUI. Re-posting there would fire at precisely the messages
    being fixed and deliver both copies: gc never cancelled the first
    send, it just could not report on it yet. `failed`/`partial` mean
    gc KNOWS something did not land, which is what makes a re-post
    clean. A hold concludes the claim and logs a `HELD` line carrying
    the receipt id, so a message that never arrives afterwards is still
    greppable against gc's own log.
  - **Anything not understood fails OPEN** — no receipt block at all
    (every gc predating gp-2rq, including the binary live today), an
    unknown status, a renamed field, a count that is not a number — is
    `receipt=unsupported` and keeps the pre-gp-32q commit-on-2xx
    behavior exactly. Only statements the adapter recognizes may
    withhold a claim's conclusion: reading schema drift as "not
    delivered" would re-post every delivery in the workspace, an
    outage strictly worse than the mangling this gate catches. So the
    pack is safe to pin BEFORE the gc rebuild, and the gate arms
    itself the moment a receipt-emitting gc is running: no flag day,
    no ordering constraint between the two fixes.
  - **Anything not understood fails OPEN**, and that now includes any
    status outside the confirmed enum. `timeout`, `error`, `rejected`
    are not statements this gc makes; admitting words that merely
    sound like failures would let a producer's schema drift re-post
    and dead-letter across the workspace. Counts must also be whole,
    non-negative numbers in matching units before they can contradict
    a status, and a block where two keys normalize to one name
    (`status` beside `Status`) reads as absent rather than as
    whichever one map order surfaced.
  - **Never during the shutdown drain.** Each gc round-trip can burn
    the full forward timeout, and two of them outrun the 20s event
    drain — a re-post still in flight when shutdown seals the spool
    could neither spool its own failure nor conclude its claim before
    process exit. While draining, an unvouched delivery goes straight
    to its failure path, which now **spools before it releases
    anything**: a copy the spool accepted CONCLUDES its `(channel, ts)`
    claim and its event id, so the parked twin and any same-event-id
    redelivery skip a message the next startup already replays; a spill
    the spool refused (sealed, disabled, full) releases both, because
    then the twin's takeover is the only recovery path left. A twin
    that skips on a drain conclusion also takes no busy mark — the
    hourglass is cleared by a reply to a delivery that is no longer
    happening, and the registry that could remove it does not survive
    the restart. The address-by-handle injection leg deliberately does
    NOT defer: a spooled line replays as a channel copy and does not
    reconstruct an addressed injection, so there is nothing durable
    behind that claim and the takeover stays the recovery path.

  What the receipt has to attest is COMPLETENESS, not arrival. At
  05:39Z on 2026-08-28 — while this fix was being written — a 223-byte
  founder message reached the mayor session as 134 bytes: 89 eaten off
  the HEAD, tail byte-exact, cut mid-word. It read as a complete
  sentence starting mid-thought, and was caught only because the cut
  landed inside a word; at a word boundary it would have been acted on
  as a corrupted founder instruction and never noticed. "It arrived" is
  not the property worth gating on, "all of it arrived" is, and that is
  what the byte counts are for. That incident is a test vector in
  `delivery_receipt_test.go`.

  Every inbound log line now carries `receipt=<id> receipt_status=…
  receipt_verdict=… receipt_bytes=<delivered>/<expected>` beside the
  existing `posted=NNch`, so the next incident splits
  adapter-vs-transport at a glance, plus `receipt_digest=` — the same
  payload digest gc prints on its own `nudge-receipt` line, so one
  delivery can be followed across both logs. Members gc names as
  undelivered are listed with their error text, truncated and capped:
  a receipt is written by another process and its error strings come
  from a terminal, so nothing in it can forge or flood a log line. `SLACK_DELIVERY_RECEIPT_GATE=off`
  disarms the gate wholesale — the escape hatch if a gc build ever
  reports receipts wrongly and the adapter starts re-posting
  deliveries that did land.

- **Slack voice clips no longer poison a channel's inbound delivery**
  (gp-xnc, 2026-08-26 09:48Z incident). Slack-native recordings
  (`subtype: slack_audio`, `audio_message.m4a`) carry an EMPTY
  `mimetype`/`filetype` — even from `files.info` — and the adapter
  passed that through as an omitted `mime_type`, which gc's
  `extmsg.ExternalAttachment` requires: a deterministic 422. The
  coalescer then restored the failed batch ahead of everything newer
  and retried it every window forever, so three founder voice memos
  and every later message in the channel never reached the mayor, at
  one 422 log line per 8s. Two fixes:
  - `mime_type` is always sent: Slack's value when present, else the
    file name's extension (`.m4a` → `audio/mp4`; a pinned table for
    the media types Slack clients produce, then the host database),
    else Slack's `filetype` code, else the `slack_audio`/`slack_video`
    subtype, else `application/octet-stream`; the field is no longer
    `omitempty`.
  - Poison handling in the coalescer: a delivery gc rejects as
    payload (HTTP 400/413/415/422) no longer retries forever. A
    rejected multi-message batch is isolated — each member re-posted
    alone under the same flush — so innocent batch-mates deliver
    immediately and only the member gc still rejects is charged; the
    probe stops at the first transient single (a hung gc must not cost
    one client timeout per member). Reactions are never posted solo and
    are charged only when their own group is posted and rejected.
    After 3 rejections the message is written to a per-channel
    dead-letter file (`SLACK_INBOUND_DEAD_LETTER_DIR`, default
    `<GC_CITY_PATH>/.gc/slack/inbound_dead_letter/<channel>.jsonl`;
    full envelope + reason, fsynced, O_NOFOLLOW, 0700/0600 enforced;
    an unconfirmed write keeps the entry buffered and retries) and the
    channel moves on. Everything else — gc unreachable, 5xx, 408/429,
    and operational 4xx such as 401/403/404 — keeps the retry-forever
    durability unchanged. `postInbound` errors now carry the HTTP
    status (`inboundPostError`) so the classes can be told apart;
    their text is unchanged.

- **Go resolver loopback latch** (gp-keg / ci-c3zl9 / hw-i6yt7,
  pc_82ff6cc9d209; citadel 2026-08-24 21:00–21:09 CT). The gp-bsk DNS
  self-heal had already flipped the Socket Mode runner to Go's built-in
  resolver during the evening's MagicDNS stalls (20:58: `lookup slack.com
  on [fd7a:115c:a1e0::53]:53: server misbehaving`). At 21:00:03 the
  MagicDNS watchdog's `tailscale set --accept-dns=false` rewrote
  `/etc/resolv.conf`; Go's resolver — which parses that file itself, in a
  package-level singleton no fresh `net.Resolver` escapes — caught the
  rewrite, dropped onto its compiled-in fallback `127.0.0.1:53` /
  `[::1]:53`, and stayed there (`lookup slack.com on [::1]:53: …
  connection refused`) through two full 12-attempt reconnect cycles until
  a manual `gc service restart slack` at 21:08:57, with the system
  resolver healthy throughout; the gp-bsk 10m self-restart would have
  fired at ~21:10. This signature now recovers in seconds:
  1. **Detect** — a connect failure whose lookup reports *exactly* one of
     Go's fallback nameservers (typed `*net.DNSError.Server` through
     `url.Error`/`net.OpError`, or slack-go's flattened `lookup X on S:`
     text). A systemd-resolved stub (127.0.0.53) or a local stub on
     another port never matches.
  2. **Escape without net's help** — on each such failure the runner
     re-parses `/etc/resolv.conf` itself, right then, and pins the
     pure-Go resolver's `Dial` hook to a usable nameserver from it
     (rotating across candidates on repeated events), so every DNS
     exchange bypasses net's cached config. The pin is transient —
     cleared on `Connected`, re-derived on the next event — so it cannot
     go stale across a later DNS change.
  3. **If the pin does not take, get out fast** — a signature that
     persists for `SLACK_DNS_LOOPBACK_EXIT_AFTER` (default 5, `0`
     disables the exit) consecutive lookups made with a pin already in
     place takes the same orderly, loss-free exit as the gp-bsk
     self-restart, and the service supervisor restarts the adapter so a
     fresh process parses the healthy file the way net expects. That
     trigger cannot restart-loop: a fresh process reproduces the
     signature only if the file yields no nameserver, and then there is
     no pin and no exit. Nothing to pin (file unreadable, no nameserver
     line, or only Go's own fallback addresses) deliberately never exits
     — a fresh process would latch on the same file and restart-loop,
     taking outbound `/publish` down with it — every attempt re-parses
     instead, keeping an earlier pin meanwhile, so the pin applies the
     moment the file is usable; the gp-bsk 10m self-restart stays the
     backstop.
  `/healthz` grows `socket_dns_pin=<ns>` while a pin is in place and
  `socket_dns_loopback_streak=<n>` while a streak is running. The
  gp-bsk 3-strike `no such host` flip and both backoff ladders are
  unchanged. (`GODEBUG=netdns=cgo` was considered and dropped: it does
  not apply once `PreferGo` is set — net honors the resolver flag before
  the GODEBUG mode — and forcing the system resolver would reverse
  gp-bsk's escape from it.)

- Urgent-path twin double-delivery (gp-ios, pc_c920ff5fe90c live shape,
  2026-08-20 06:44Z): a bot-mention pair (`message` + `app_mention`,
  same ts, distinct event ids) raced BOTH copies down the urgent
  channel path concurrently — the event-id dedup can't collapse the
  pair, gc's transcript append dedups only the RECORD, and gc's member
  notification is built from each POST body with no dedup of its own,
  so the bound session read the same message id twice in one turn
  (once bare, once wearing the thread-context preamble). A new
  per-(channel, ts) delivery claim serializes the channel copy: the
  first twin owns the POST, a concurrent twin parks until the owner
  concludes and then drops (owner delivered) or takes over (owner
  failed — never a loss path). The coalesced-batch path claims each
  member the same way, closing the sliver between an urgent twin's
  conclusion and its deliveredIDs record; a claim still in flight at
  batch time is kept without parking (fail open to a duplicate in that
  narrow window, never to loss).
- Coalesced-batch restore corruption (gp-ios, latent): the batch
  delivery filters compacted the batch slice IN PLACE while the
  caller restores its own slice on failure — a dropped member plus a
  failed POST re-queued a corrupted batch (tail entries duplicated,
  dropped-position members lost) for the timer retry. The filters now
  build fresh slices.
- reply-current send-pipeline failures now always leave a
  machine-readable JSON envelope on stdout (gp-ios, pc_7fe644e666a6):
  a session-resolution or publish failure used to surface as a bare
  SystemExit message on stderr with an EMPTY stdout — "non-JSON
  (empty/error)" to the calling agent, indistinguishable from a
  crashed script. Failures keep exit code 1 and the stderr line but
  stdout now carries `{"delivered": false, "stage": …, "error": …}`.
  Usage errors and company-turn contract errors still raise SystemExit
  (the message is the product there). The accidental-mrkdwn guard
  (gp-o42) additionally fails OPEN: any fault inside it sends the body
  unguarded with a stderr warning instead of blocking the send, and
  new tests pin the guard's behavior on CJK + curly-quote bodies
  (including U+FF5E/U+301C tilde lookalikes, which are never touched).
- Reply-current turn anchoring (gp-6j3; fleet repro 8/20, three misfires
  in one hour across two cities): the default threading scanned for the
  LATEST inbound at send time, and coalesced delivery + interleaved
  channel/thread traffic made that a different message than the one the
  session was answering — threaded asks got answered top-level, and one
  reply landed in a foreign thread outright. New `--turn-ts <ts>` flag
  on `gc slack reply-current` pins the anchor to the exact inbound being
  answered (its thread root when threaded, channel level when not) via a
  transcript lookup by ts; an unresolvable ts is a hard error with
  explicit-anchor guidance (`--reply-to` / `--no-thread`), never a
  silent top-level post. The registered reply template now renders
  `--turn-ts {message_ts}` into every gc inbound reminder, the
  once-per-channel how-to documents the flag, and the coalesced-block
  header steers replies to older batch members (which have no transcript
  entry of their own) through `--reply-to`/`--no-thread`. A live company
  turn refuses `--turn-ts` and points at `--turn-ref`. The no-flag
  latest-inbound inheritance (gp-i62) is unchanged as a fallback.
- Coalescer twin double-delivery (pc_c920ff5fe90c, folded into gp-6j3):
  a bot-mention pair (`message` + `app_mention`, same ts, distinct
  event ids) could split when the bot user id was unknown — one copy
  buffered as chatter, the other took the urgent path — and the flushed
  batch's `slack-batch-…` dedup key can never collide with the urgent
  copy's `slack-<ts>`, so gc delivered the same message id twice in one
  turn with different decoration. Flush-ahead now withholds the urgent
  message's own ts from the batch (restored for the timer retry if the
  urgent post then fails — never dropped), a ts already delivered to
  the channel audience is skipped at enqueue AND filtered again at
  batch-delivery time (narrowing the race where the twin buffers while
  the urgent POST is still in flight to the POST's own in-flight
  window; an urgent failure never records its ts, so the buffered copy
  still delivers — the residual is fail-open to a duplicate, never to
  loss, accepted like gp-729's documented window limitations), and
  same-ts entries collapse inside a batch.

### Added

- Human-reaction visibility (gp-by3): the switchboard app subscribes to
  `reaction_added`/`reaction_removed` (manifest: both events + the
  `reactions:read` scope — **live apps must add these in their Slack
  dashboard config and reinstall**; the repo manifest does not update
  installed apps) and forwards each human reaction to the
  conversation-bound session as a lightweight tagged notification:
  reactor, emoji, target message ts, best-effort target snippet
  (`conversations.replies` lookup, degrades to ts-only), threaded under
  the target's thread when it has one. Rooms ride the gp-729 coalescer
  (a lone 👍 still wakes the session after the window — the ack that is
  a thread's only answer now arrives); DMs and coalescing-disabled
  deployments forward immediately. Mechanical fleet traffic never
  echoes: the adapter's own reactions, anything using the configured
  busy emoji (`BUSY_REACTION`, default `hourglass`), and reactors
  matching a `peer_bots.json` entry's `bot_user_id` all drop. The
  notification is non-anchoring by construction (`Actor.IsBot=true`,
  `"reaction: "` display prefix; the intake helpers and the coalesced-
  block anchor line exclude bot-tagged entries), so `reply-current` /
  `react` / `upload` never anchor on a reaction. Company rooms and
  per-agent DMs are out of scope v1.

- **Inbound liveness now reaches the gc service health model** (gp-rol;
  papercuts pc_5ede9badf7b1 / pc_fc584c0e47ba). During the 2026-08-19
  outages `gc service list` reported `state=ready` for 26 minutes of dead
  inbound. `/healthz` now stays 200 (so gc keeps routing outbound
  `/publish`, the one path that kept working) but carries advisory
  `X-GC-Health: degraded` + `X-GC-Health-Reason` headers whenever the
  inbound-liveness watchdog has a confirmed stall or the Socket Mode
  transport has been down for over 2 minutes (including never-connected
  starts, e.g. a rejected `SLACK_APP_TOKEN`). A gc with the matching
  proxy_process support (fork `integration`) renders that as
  `State=degraded` on `gc service list` / the dashboard — the reason on
  `gc service show slack` and the API — while `LocalState` stays
  `ready`; older gc binaries ignore the headers.

- Inbound token-efficiency pass (gp-729, the Aug-17 ranked list):
  1. **Burst coalescing** — untargeted, non-bot-mentioned channel
     messages buffer for a short debounce (`SLACK_COALESCE_WINDOW`,
     default 8s; 0 disables) and deliver as ONE inbound whose text
     block carries every message verbatim (sender, ts, thread
     annotations), instead of N full system-reminder blocks. Targeted
     or bot-mentioned messages keep the exact busy/alias/dedup flow and
     flush any pending buffer ahead of themselves so ordering holds. A
     single-message flush is byte-identical to the immediate path.
  2. **Thread-parent dedup** — the adapter now tracks delivered message
     ids per audience; thread-context preambles collapse priors the
     audience already received to a one-line count ("N earlier messages
     already delivered … not re-quoted") and keep the full quote only
     for messages the audience genuinely hasn't seen.
  3. **Reply boilerplate once** — the adapter registers a one-line
     reply-instruction template with gc (`reply_instructions` on
     adapter registration), replacing gc's generic three-line
     reply-current fallback on every reminder; the full how-to (file
     mechanics, threading, react, channel name+id) is appended once per
     channel per adapter lifetime to that channel's first delivery.
  4. **Channel names next to ids** — pack-owned wrapper blocks
     (coalesced-burst headers, the once-per-channel how-to, the
     alias-dispatch reminder) render `#name (Cid)` via a new
     conversations.info cache (mirrors the users.info cache; raw id on
     any failure; needs `channels:read`).
  5. **Alias-dispatch turn-dedup** — a targeted inbound whose alias
     session already holds an active gc binding for the originating
     conversation no longer gets a second direct session-message copy
     (which landed the same message id twice in one turn); the
     `@handle:` address marker is restored into the channel copy so
     addressed-ness survives. Binding lookups are cached (60s) and fail
     open to double delivery.
  6. **Per-channel delivery policy** — new `delivery_policy.json`
     registry (`SLACK_DELIVERY_POLICY_PATH`, SIGHUP-reloadable):
     `{"channels": {"C…": {"mode": "digest", "interval_minutes": N}}}`
     switches a channel to verbatim batched delivery every N minutes
     (nothing dropped or summarized); every unlisted channel — and an
     absent file — stays immediate, so day one is behavior-neutral.
  Alias-dispatch reminders now render the originating channel as
  `#name (Cid)`. Adapter restart required; no script-side changes.

- Peer-bot visibility on the legacy channel path (gp-kop, Afik-approved
  Option A): messages authored by an EXPLICITLY ALLOWLISTED fleet app
  (new `peer_bots.json` registry, `SLACK_PEER_BOTS_PATH`, SIGHUP-
  reloadable) are delivered to the channel-bound session as tagged
  read-only context instead of being dropped — fixing fleet mayors
  double-answering humans because they could not see each other's
  posts. Default is no wake: peer posts buffer per channel (newest 20)
  and ride ahead of the next naturally forwarded inbound; channels in
  `immediate_channels` forward each peer post immediately as its own
  inbound. Loop + safety rules are hard-enforced: every candidate
  resolves through `bots.info` (reusing the company gateway's author
  resolver) and a bot's own posts are never delivered back to itself,
  even when misconfigured into the allowlist; unknown bots keep the
  historical drop byte-for-byte; delivered posts carry a
  `peer-bot <label>` provenance tag in both the text block and the
  inbound Actor (`is_bot: true`); and no auto-response semantics exist
  anywhere in the path — the peer branch never parses targets, never
  busy-marks, never alias-dispatches, and the intake helpers now skip
  bot-authored inbounds so `gc slack reply-current`/`react`/`upload`
  can never anchor on a peer post. Adapter restart required (registry
  wiring + inbound hook); the intake-helper exclusion is script-side.

- Socket Mode inbound transport (gp-3og / ci-mk4qj): with an `xapp-…`
  app-level token (`SLACK_APP_TOKEN`, scope `connections:write`) the
  adapter dials out to Slack over a WebSocket (slack-go `socketmode`)
  and receives events, slash commands, and interactive payloads with no
  public Request URL — removing the funnel/Events-API single point of
  failure behind the 2026-08-19 silent inbound outage. Envelopes run
  through the same handlers as the HTTP path (same routing, event_id
  dedup, company admission, busy reactions, load-shedding) via a
  context-scoped trusted-transport marker that no network request can
  forge; acks are sent on 2xx handler outcomes, envelopes are left for
  Slack to redeliver on 5xx, and slash/interactive JSON bodies ride
  back as ack payloads. Auto-reconnects with backoff; each (re)connect
  backfills the gap from channel history. `SLACK_SOCKET_MODE=auto|on|off`
  is the policy knob (`off` = rollback lever); the Events API listener
  stays up either way, so cutover/rollback is the Slack-app UI flip.
- Inbound-liveness watchdog + backfill (gp-3og / ci-mk4qj): the adapter
  now tracks last-inbound time and per-channel watermarks (persisted at
  `SLACK_LIVENESS_STATE_PATH`); after `SLACK_LIVENESS_STALL_AFTER`
  (default 10m) of silence it probes watched channels'
  `conversations.history` (+ fresh thread replies) with the bot token
  and, if humans posted messages the adapter never received, raises a
  loud `INBOUND LIVENESS ALARM`, flips `/healthz` to
  `inbound_liveness=stalled`, optionally posts an alarm to
  `SLACK_LIVENESS_ALERT_CHANNEL`, and replays the missed messages
  through the normal inbound pipeline (bounded by
  `SLACK_BACKFILL_MAX_WINDOW`, deduped against live deliveries in both
  directions) — closing the exact "dead transport looks like a quiet
  workspace" gap that made the outage silent. Restart gaps backfill
  from the persisted watermarks. `/healthz` gains `socket_mode=…` and
  `inbound_liveness=…` status lines.

- Accidental-mrkdwn guard on the send path (gp-o42): `gc slack
  reply-current` (legacy and company paths), `publish`,
  `publish-to-channel`, `upload --initial-comment`, and `delegate` now
  neutralize tildes that would pair into unintended Slack strikethrough
  ("~$58.5k … ~$16.5k" rendered half a runway summary struck through).
  Slack mrkdwn has no escape sequence, so accidental delimiter tildes
  are substituted with the visually identical U+223C TILDE OPERATOR —
  but only on lines where a pair could actually form: lone tildes keep
  their ASCII byte (`cd ~/repo` survives copy-paste), code spans are
  never touched, a tilde before an optionally-signed digit/currency
  (`~$5k`, `~-$13.5k`, `~9/2`) is never a delimiter, and deliberate
  tight-wrapped `~word~` strikethrough still renders. `*bold*`,
  `_italics_`, and bullets are unaffected. New `--raw` flag on all five
  commands sends the body verbatim. Pure script-side change — no
  adapter restart needed.

  Two limits worth knowing. "Lone tilde" is per rendered line, so two
  home-relative paths on one line (`rsync ~/a ~/b`) are a pairable pair
  and both get substituted; wrap twin paths in a code span, or use
  `--raw`, when the exact bytes matter. And `gc slack post-message` is
  deliberately out of scope: its milestone summary, field values, and
  rollup items are rendered as mrkdwn by the Go Block Kit renderer,
  downstream of this Python guard, so a payload carrying
  "~$58.5k … ~$16.5k" still strikes through (titles are `plain_text`
  and unaffected). That surface predates this change and guarding it
  would need a second implementation of the heuristic in Go plus a
  `gc-slack-cli` rebuild; tracked as gp-x5bdy.

  Scope is this pack only. The sibling packs `slack-channel` (same
  `publish` / `publish-to-channel` / `reply-current` verbs) and
  `slack-mini` (`post-message`) carry no guard, so the same `gc slack`
  verb behaves differently depending on which pack a city installed.
  Porting was left out deliberately to keep this change script-side
  with no adapter rebuild; guard-port vs. documented exclusion is
  tracked as gp-cbxzk.

### Fixed

- The adapter builds and its tests pass on darwin again. The confined-open
  walk called `syscall.Openat`, which exists only on Linux — darwin's
  libSystem-based `syscall` package exports neither `Openat` nor
  `SYS_OPENAT` — so the entire adapter failed to compile on macOS. The
  walk now binds `golang.org/x/sys/unix`; every flag constant and syscall
  it swaps is a value-identical mirror on Linux, so behavior there is
  unchanged. Three `readConfinedFile` tests also failed on macOS, where
  `TMPDIR` lives under `/var`, itself a symlink to `/private/var`:
  `confineFileUploadPath` EvalSymlinks-resolves only the *root* and
  documents the path as caller-pre-resolved, so a fixture handing it a
  raw `t.TempDir()` broke that contract and made every path — including
  genuinely in-root ones — read as an escape. The fixtures now resolve
  their temp dir once through `tempDirResolved`. A harness defect, not a
  confinement one: no production behavior changed.
- Inbound files recorded inside Slack itself — voice clips (file subtype
  `slack_audio`) and video clips (`slack_video`) — arrive with an empty
  `mimetype` AND `filetype`, which the adapter passed through verbatim.
  The inbound payload then omitted `mime_type`, a REQUIRED property of
  gc's `extmsg.ExternalAttachment`, so gc rejected the whole message
  with 422 and the recording plus its text caption silently never
  reached the bound session. The adapter now derives a media type for
  every inbound file (Slack's `mimetype` → file-name extension → Slack
  `filetype` code → Slack-native subtype → `application/octet-stream`)
  and always serializes `mime_type` (no longer `omitempty`), so a
  payload can never again be rejected for a missing key.
  Scope: this covers the legacy `/extmsg/inbound` delivery path. Files
  shared in a company/multi-bot room take the company hydration path,
  which never posts to `/extmsg/inbound` and still renders Slack's raw
  `filetype`, so a Slack-native recording there is unchanged by this fix
  and still lists an empty filetype.
- The `[[service]]` proxy_process command pointed straight at
  `adapter/gc-slack-adapter`, a gitignored build artifact — but
  `gc import install` re-materializes the pack cache git-only, so every
  pack pin bump wiped the binary and left the slack service dead until
  someone rebuilt it by hand. The service command is now
  `adapter/run.sh` (checked in, survives every materialization): it
  sources the secrets env file, execs an existing binary untouched,
  and on a missing binary finds a Go toolchain (PATH plus
  Homebrew/system fallbacks for minimal supervisor environments),
  rebuilds from the colocated sources with loud stderr logging,
  publishes via PID-suffixed temp file + atomic `mv` (safe under
  concurrent supervisor restarts), and execs.

  Env-file handling distinguishes the two cases. An absent *default*
  (`~/.config/gc-slack-adapter/env`) warns and continues, since
  supervised deployments legitimately inject the env another way and
  the adapter fail-fasts on missing required keys itself. An absent
  path you set *explicitly* in `GC_SLACK_ADAPTER_ENV` is fatal: the
  adapter validates that credentials are present, never where they
  came from, so continuing would boot it against whatever Slack token
  happens to be in the ambient environment.

  Because the rebuild now sits on the service startup path, it is
  hardened for the minimal supervisor environments it targets: the
  toolchain search also covers `/usr/bin/go` and `/bin/go` (where a
  distro Go lands, and which a restricted PATH hides); `GOCACHE` and
  `GOPATH` are defaulted under `TMPDIR` when HOME, `XDG_CACHE_HOME`
  and `GOCACHE` are all unset, which `go build` otherwise rejects
  outright; and the toolchain is checked against `go.mod`'s `go`
  directive up front, so a too-old Go under `GOTOOLCHAIN=local` fails
  with a named remedy instead of a raw compiler error (the default
  `GOTOOLCHAIN=auto`, which can fetch the required toolchain itself,
  warns and proceeds).

### Added

- `gc slack upload` accepts `--conversation-id` (plus `--kind
  dm|room|thread`, default `room`), the file-side twin of
  `publish-to-channel` (ci-ta49 / gp-8z7): an explicit channel id
  skips the extmsg-binding lookup and posts the file straight to the
  adapter's `/publish-file` (files-upload-v2), so sessions with no
  binding — mayor, chief-of-staff — can attach files to any channel
  they were addressed from. Implies `--via adapter` (gc's
  outbound-file endpoint requires a binding; combining with `--via
  gc` errors), rejects `--thread-current` (its latest-inbound lookup
  is binding-oriented; pass `--thread-ts` explicitly), and mirrors
  publish-to-channel's receipt gate — exit 1 when the adapter
  returns `delivered=false`. The default binding paths are
  unchanged, including their exit-0-on-undelivered contract (now
  pinned by a regression test).

- `gc slack read` (gp-lie): native channel + thread history reads via
  `conversations.history`/`conversations.replies` using the pack's bot
  token, closing the last claude.ai-MCP dependency for Slack reads —
  history recovery now works during MCP outages and even when the local
  adapter is down. Channel mode returns the newest `--limit` messages
  rendered oldest → newest; `--thread-ts` reads a thread from its
  parent; `--oldest`/`--newest` bound the window; cursor pagination and
  HTTP-429 `Retry-After` retries under the hood. Authors resolve via
  `users.info` (`--no-names` to skip), attachments print as pointers
  and `--download` spools them Bearer-authed into the adapter's
  `$INBOUND_FILE_STORE/<channel>/<ts>-<name>` layout behind the same
  Slack-host allowlist as the adapter (https, port 443,
  `*.slack.com`/`*.slack-files.com`, redirects refused so the token
  can't follow a 3xx off-host). `not_in_channel`, `channel_not_found`,
  `thread_not_found`, `missing_scope`, and auth failures map to
  actionable messages. Deliberately no `gc slack search`:
  `search.messages` accepts only a user token with `search:read` and
  the pack stays bot-token-only (the app must never act as a human
  user); the limitation is documented in the verb help instead.

### Fixed

- `slack_intake_common._maybe_load_adapter_env` now strips the shell
  `export ` prefix when parsing the adapter env file. The live file is
  written shell-style (`export SLACK_BOT_TOKEN=...`, sourced by the
  adapter's run.sh), so every key previously parsed as `export KEY` and
  the loader silently loaded nothing — unnoticed until `gc slack read`
  became the first pack command to need `SLACK_BOT_TOKEN` in-process.

- Busy-reaction lifecycle (hq-xizo, ported from
  `feat/hq-xizo-slack-full-hardening`): a targeted inbound gets a busy
  reaction (`BUSY_REACTION`, default `hourglass`; set-but-empty
  disables) added on dispatch and removed when the agent's reply
  publishes back into the same conversation/thread, tracked in a
  bounded in-memory registry keyed by (conversation, thread key) with
  a 30-minute TTL. Replaces the prior unconditional `eyes` reaction on
  targeted inbounds; the ⚠️ dispatch-failure reaction stays. `/react`
  gains an optional `remove:true` issuing `reactions.remove`
  (`no_reaction` treated as benign delivery, mirroring
  `already_reacted`), surfaced on the CLI as `gc slack react
  --remove`. Beyond the original hq-xizo commit, the removal is
  ordered after the corresponding `reactions.add` completes (a fast
  reply otherwise races the in-flight add and the busy emoji sticks
  forever) and a threaded `/publish-file` reply clears the mark
  exactly like a text publish. A thread-reply inbound is marked under
  both its thread root AND its own ts — reply-current and the
  alias-dispatch instructions thread replies under the inbound's own
  ts, which the root-only key missed. A channel-root reply (the
  documented default `gc slack reply-current` shape, no thread ts)
  clears every pending mark in the conversation, and re-targeting a
  thread before the first reply lands removes the displaced message's
  reaction instead of stranding it. The mark registers before the
  forward to gc (a reply can arrive before postInbound even returns;
  a failed forward cancels the mark before releasing the event's
  dedup claim and restores any marks the failed inbound displaced),
  displaced-mark reactions are removed only after the displacing
  forward succeeds, and a re-mark of the same message merges
  add-completion channels so a remove waits for every in-flight add
  (the wait bound exceeds the Slack client timeout, so ordering
  provably holds). Registry entries carry displaced-ancestor
  reactions across overlapping failed re-targets (deduplicated,
  bounded per entry) so every added emoji stays clearable; displaced
  marks are retired only once the event's FINAL delivery succeeds
  (the alias POST when one fires, postInbound otherwise) and restored
  if it fails — unless a reply consumed the thread while the dispatch
  was in flight (consumed keys are tombstoned; blocked marks get
  their reactions removed instead of being re-parked unclearably);
  and a failed cross-channel alias delivery removes its own busy
  emoji next to the ⚠️ — synchronously, before any parked retry
  wakes — instead of stranding it. Note the lifecycle fires only when an agent target
  is
  parsed from the message (`@handle:` prefix, User Group mention, or
  sticky thread handle) — plain messages that reach a session solely
  through a channel binding carry no parsed target and get no busy
  reaction, by design. (hw-94w5k finding #3) gp-4vq widens the gate to
  @-mentions of the adapter's own bot user — see Fixed below.

### Fixed


- **A transient network blip can no longer become an hours-long inbound
  outage** (gp-bsk; incidents 2026-08-22 12:59 CT and 2026-08-23
  13:54Z–19:12Z, the latter 10.5h of dead inbound). Three layers, all in
  the Socket Mode runner:
  1. *Reconnect backoff is now hard-capped at 2 minutes.* slack-go
     v0.29.0's internal reconnect ladder is effectively unbounded (its
     `Max` is applied only on integer overflow), and the 8/23 outage sat
     in observed waits of 27m/55m/1h49m after the network had already
     recovered. The runner now kills the client cycle the moment the
     reported internal backoff exceeds the ceiling and lets its own
     outer ladder (floor 5s, cap 2m, was 5m) own the pacing —
     `apps.connections.open` is cheap and every reconnect is loss-free
     via the watermark backfill, so hour-scale patience is never
     correct.
  2. *DNS self-heal.* Three consecutive `no such host` connect failures
     (the 8/22 poisoned-resolver signature) stop trusting the in-process
     resolver: the runner flips — stickily — to a pure-Go resolver that
     re-reads `/etc/resolv.conf` (wired through both the
     `apps.connections.open` HTTP client and the WebSocket dialer) and
     rebuilds the client immediately. If the transport has connected at
     least once this process and then stays dark past
     `SLACK_SOCKET_SELF_RESTART_AFTER` (default 10m, 0 disables) across
     repeated failures, the adapter runs its orderly shutdown (the
     gp-9e7 drain: buffered inbounds reach gc or the durable spool —
     never a bare `os.Exit`, which would strand anything the liveness
     backfill had just admitted below the watermark) and exits 1 so
     the service supervisor restarts it — spool replay + startup
     recovery + watermark backfill make the bounce loss-free (proven in
     both incidents). Never-connected
     misconfigurations (Socket Mode toggle off, bad app token) still
     only degrade health — no restart loop.
  3. *The inbound-liveness ALARM now escalates.* Its only consumer used
     to be the local log (and the 8/23 alarm at 18:47Z fired correctly
     into the void); with the socket down it now flips the runner into
     aggressive reconnect — outer backoff reset to the floor, any
     backoff sleep (internal or outer) abandoned — since the alarm is
     positive evidence messages are being missed. `/healthz` grows
     `socket_fresh_resolve=true` once the DNS self-heal has tripped.

- Pack pin bumps can no longer strand the adapter service (gp-d7l):
  the `[[service]]` command is now `adapter/run.sh` (checked in)
  instead of the gitignored built binary. `gc import install`
  re-materializes the pack cache git-only, which used to wipe both the
  deployed env-wrapper shim and `gc-slack-adapter.real` and leave the
  supervisor fork/exec-ing a nonexistent path until a human rebuilt by
  hand (outages 2026-07-30 and 2026-08-05, 15:37–15:51Z). run.sh now
  sources the secrets env file (absorbing the shim's job), execs an
  existing binary unchanged (idempotent fast path), and when the
  binary is missing self-heals: locates a Go toolchain (PATH plus
  Homebrew/system fallbacks for minimal supervisor environments),
  rebuilds `gc-slack-adapter.real` from the colocated sources with
  loud stderr logging, publishes it with an atomic rename safe under
  concurrent restarts, and execs it.

- `gc slack reply-current` now inherits the thread from the latest
  inbound (gp-i62): a thread-reply inbound's transcript entry carries
  the Slack `thread_ts` in `ReplyToMessageID`, and the reply anchors
  there by default — including when `--conversation-id` names the same
  conversation explicitly, which previously always posted at channel
  level (live burns 2026-08-09, #gastown + #fundraising). An unthreaded
  inbound keeps the channel-level reply; an explicit target naming a
  *different* conversation never borrows the inbound's thread anchor;
  a failed lookup degrades to channel level with a stderr warning
  (`--via adapter` must survive a gc outage). New `--no-thread` flag
  forces a channel-level post. `--thread-current` now anchors at the
  thread ROOT when the latest inbound was itself a thread reply —
  Slack threads hang off the parent ts, so anchoring at the child
  stranded the reply. Retires the fleet-memory workaround of routing
  threaded replies through `publish-to-channel --thread-ts`.
- `slack_intake_common._maybe_load_adapter_env` now strips the shell
  `export ` prefix when parsing the adapter env file. The live file is
  written shell-style (`export SLACK_BOT_TOKEN=...`, sourced by the
  adapter's run.sh), so every key previously parsed as `export KEY` and
  the loader silently loaded nothing — unnoticed until `gc slack read`
  became the first pack command to need `SLACK_BOT_TOKEN` in-process.
- The busy reaction now fires when a human @-mentions the adapter's
  bot user (gp-4vq): live traffic showed the affordance never fired in
  practice because nobody addresses agents with the `@handle:` prefix
  syntax — real messages tag the bot with Slack's native `<@U…>`
  mention, which parsed as no-target. A mention of the adapter's OWN
  bot user id (read from the event envelope's `authorizations` block,
  is_bot-gated; the `app_mention` event type is the fallback signal
  when a delivery omits it) anywhere in the text now makes the inbound
  busy-eligible. Routing is deliberately untouched: no `ExplicitTarget`
  is fabricated (a synthetic target would read as "addressed to someone
  else" to the channel-bound session and mute it) and no alias dispatch
  fires. Slack delivers a bot mention twice — `message` + `app_mention`
  events with distinct event_ids for one ts (hw-vzd5y edge case 2, now
  with production evidence) — and both deliveries mark the same ts:
  the registry's same-message merge keeps a single mark, the second
  `reactions.add` is Slack's benign `already_reacted`, and the reply
  removes the reaction exactly once. The inbound log line gains
  `bot_mention=%t` for live verification. Binding-routed generic
  messages (no tag at all) still get no busy reaction — gc-side
  routing feedback for those is the tier-2 follow-up spec'd in
  hw-94w5k notes.
- Inbound messages now resolve Slack user ids to display names
  (hq-uxln9, ported from slack-mini's hq-fh9 fix): the sender line in
  gc's injected reminder shows a human name instead of a raw id like
  `U0AN32RPBFT`, inline `<@U…>` mentions in forwarded body text are
  rewritten to `@display-name`, and thread-context preamble author
  lines resolve the same way. Backed by a users.info lookup with an
  in-memory TTL cache (1h success / 5m negative); any lookup failure
  falls back to the raw id (mention tokens are left verbatim, or use
  their `<@U…|label>` label when present). Requires the `users:read`
  scope — without it, behavior is unchanged. The alias-dispatch
  (`@handle:`) system-reminder's sender line renders the same way —
  `by user Afik Cohen (U0…)` instead of the bare id. The
  operator-curated `slack-user-aliases.json` map is consulted first
  (inverse lookup, id → handle): a curated identity renders as its gc
  handle with no Slack call at all, so locked-down workspaces missing
  `users:read` still get names for the identities the operator cares
  about.
- Slack Events API redeliveries are now deduplicated on `event_id`
  with a 10-minute seen-set: Slack retries any delivery it considers
  unacknowledged and each retry re-forwarded the same message into the
  bound session as a duplicate notification (observed as
  byte-identical inbound log pairs). Entries are two-state
  (in-flight → committed): a redelivery racing the first delivery's
  still-running forward parks (slot released, no dispatch-semaphore
  starvation) until the owner's verdict — commit drops it, forget
  hands it the event — and never gives up: it already returned a 200,
  so Slack will not resend it. The wait runs in a goroutine after the
  handler has returned, so Slack's ack is never delayed behind it.
  Adapter→gc forwards run on a 20-second-timeout client and Slack Web
  API calls on 30s/120s-timeout clients so claims always conclude,
  and a take-over blocks for dispatch capacity instead of dropping
  the event's last copy on a full queue. For targeted inbounds the alias dispatch
  owns the verdict (success commits, failure forgets). Deliveries
  dropped at the queue-full boundary are never recorded, and a failed
  forward releases its id, so a Slack retry can always recover the
  message. Redeliveries of known events are routed past the
  queue-full load-shed (a parked wait needs no dispatch slot), so a
  saturated queue can never discard the only remaining copy of an
  in-flight event. Transient `@@handle` launcher failures (spawn or
  first-message forward) forget the claim like every other forward
  failure — the launcher alias registers only after a first message
  lands (on any attempt, including a retry that re-acquired the
  thread session), so a retry re-enters the launcher instead of
  dead-ending in the pre-claimed branch; terminal launcher outcomes
  (delivered, user-error ephemerals) commit. (hw-94w5k finding #4)
- `openBeneath` (the confined open backing `/publish-file`) restores
  the old per-component no-follow guarantee on top of `os.Root`
  (which follows a symlink at the root argument and resolves in-root
  symlinks at every component, ignoring `O_NOFOLLOW` for them): the
  root is pre-opened with `O_NOFOLLOW|O_DIRECTORY` and
  identity-matched against the `os.Root` handle, every intermediate
  component is Lstat'd (symlink → hard failure) and descended into
  via a pinned sub-root whose identity must match, and the leaf is
  Lstat'd + inode-pinned — so a raced-in link anywhere on the path
  fails instead of substituting a different (even in-root) file.
- Human file posts delivered with subtype `file_share` are no longer
  discarded by the system-noise subtype gate, and file-only posts (no
  caption) are no longer discarded by the empty-text gate — both now
  reach the attachment download pipeline. Other subtypes
  (`message_changed`, `bot_message`, …) and bot-authored file posts
  stay filtered. (hw-94w5k finding #1)
- `gc slack react` / `reply-current` latest-inbound lookup now matches
  `target_session` against every identifier the session is known by
  (id, `GC_SESSION_NAME`, gc-reported alias/session_name), not just
  `GC_SESSION_ID`. Name-bound sessions produce inbound events carrying
  the NAME, so the id-only match made the default `--current` mode
  bail with "no recent inbound transcript entry" right after an
  inbound landed. The ambient `GC_SESSION_NAME` only joins the match
  set when the requested session IS the current one — an explicit
  `--session <other>` never inherits it. (hw-94w5k finding #2)

### Changed

- The adapter module takes its first dependency, `golang.org/x/sys`
  (hash-pinned in `adapter/go.sum`), because it is the only maintained
  source of a darwin-capable `Openat`. The module's zero-dependency
  property was traded away deliberately; the rejected alternatives and
  the reason are recorded in `adapter/confined_open.go`. One
  operator-visible consequence: `adapter/run.sh`'s build-on-missing
  self-heal used to rebuild the binary with no module downloads at all,
  and now needs either a warm module cache or GOPROXY egress the first
  time it builds. On a host behind an egress allowlist, pre-warm with
  `go mod download` in `slack-full/adapter/` before relying on the
  self-heal — a cold cache there fails the rebuild with `module lookup
  disabled by GOPROXY=off`, and the remedy `run.sh` prints on failure
  hits the same wall.
- **Behavior change on upgrade:** the slack service now *exits 1 at
  startup* when `GC_SLACK_ADAPTER_ENV` is set to a path that does not
  exist, where it previously logged one warning and started on the
  ambient environment. Pointing the variable at a nonexistent path is
  this pack's own established idiom for "no env file", so a unit file or
  wrapper using it that way for the **service** will stop starting after
  this pin bump. Audit before upgrading with
  `grep -r GC_SLACK_ADAPTER_ENV` across your unit files and wrappers, and
  either create the file or unset the variable to fall back to the
  default. The strict rule is service-only: `slack_chat_*` commands
  (`scripts/slack_intake_common.py`) still treat a missing path as "no
  env file".
- Renamed the pack directory from `slack-pack/` to `slack-full/` and the
  `pack.toml` name from `slack` to `slack-full` as part of the Slack pack
  tiering split (`gc-yrw`). This pack is now **Tier 3** of the Slack
  family; the smaller [slack-mini](../slack-mini) (Tier 1) and
  [slack-channel](../slack-channel) (Tier 2) packs were extracted from it.
  The user-facing verb surface (`gc slack <cmd>`) and the registered
  `slack` service name are unchanged — only the catalog directory and pack
  name moved, so there is no collision with `slack-mini` / `slack-channel`.
  See the [tiering design memo](../docs/design/slack-pack-tiering.md) and
  the README "Tiering" section for the decision tree. (`gc-yrw.5`)
- Moved CLI commands (`gc slack import-app`, `map-channel`, `map-rig`,
  `sync-commands`, `enable-room-launch`, `post-message`) from the gc
  binary into a new in-pack Go module at `examples/slack-pack/cli/`
  (module `github.com/sjarmak/gc-slack-cli`). User-facing surface
  (`gc slack <cmd>`) is unchanged — pack wrappers under
  `commands/<cmd>.sh` exec the new binary at
  `$GC_PACK_DIR/cli/gc-slack-cli` so operator command-line ergonomics
  stay identical to the pre-relocation gc-binary verbs. The pack now
  ships two Go binaries (adapter + cli), each in its own go.mod;
  `gc slack status` continues to dispatch to the existing Python
  implementation under `scripts/slack_chat_status.py`. Build flow
  documented in [CONTRIBUTING.md](./CONTRIBUTING.md#build-flow).
  (`gc-coe10`)

### Added

- Outbound publishes carrying an idempotency key are stamped with a
  low-visibility reference marker (`_ref:<12 hex>_`) so a later reader can
  find the exact message in Slack. The in-process dedup cache only lives
  for its TTL and does not survive a restart, so it cannot provide durable
  delivery evidence; the marker can. The readback contract — what a marker's
  absence does and does not mean, and where a reader has to look — is stated
  normatively in the `referenceMarker` doc comment in `adapter/main.go`, and
  this entry deliberately does not restate it, so there is one place to edit.
  Two properties worth knowing without opening the source: absence always
  means UNKNOWN, never undelivered (all history predating this change is
  unmarked); and a marker on a threaded reply is not reachable from
  `conversations.history`, which does not return thread replies.

  Note on Slack's ceiling, since the obvious assumption is wrong: Slack does
  not reject text over 40,000 — it truncates from the end and still answers
  `ok:true`. The marker is the tail of the text, so an oversized post would
  deliver a mangled partial marker under a receipt that looks perfect. The
  advertised `MaxMessageLength` therefore reserves the marker's 20 bytes, and
  `handlePublish` enforces the same budget itself rather than trusting an
  advertisement nothing in gc reads today. Over the budget the caller's text
  is posted **unstamped** rather than trimmed: the message is the caller's,
  the bookkeeping is ours, and an honest absence is already something the
  contract requires readers to tolerate. When Slack reports a truncation
  anyway, the publish receipt carries `metadata["truncated"]="true"`
  (`dr-3msk6.3`).
- `gc slack retry-peer-fanout` — operational recovery for peer-fanout.
  Walks recent `extmsg.peer_fanout_failed` events (added in this change
  too), filters by `--since` / `--conversation` / `--max`, deduplicates
  against successful `extmsg.peer_fanout_retried` events, and re-issues
  each notification via the new
  `POST /v0/city/<cityName>/extmsg/peer-fanout/retry` endpoint with a
  small cooldown between attempts. The endpoint emits an
  `extmsg.peer_fanout_retried` audit event per attempt with the
  `original_seq`, so re-running on the same set is a no-op
  (`gc-cby.7`).
- SIGHUP-driven reload for the four CLI-written registry files
  (`apps.json`, `channel_mappings.json`, `rig_mappings.json`,
  `room_launch_mappings.json`). Operators can now run
  `gc slack import-app`, `gc slack map-channel`, `gc slack map-rig`, or
  `gc slack enable-room-launch` and signal the adapter with
  `pkill -HUP gc-slack-adapter` (or any other SIGHUP delivery) to pick
  up the new bindings without a service restart (`gc-cby.23`). Reload is
  all-or-nothing across the four registries — a single parse failure
  aborts the cycle with the live state untouched. A missing file is a
  no-op (preserves state); operators clear by writing an empty `{}`
  document, NOT by `rm`.

### Changed

- The trailing reminder printed by `gc slack map-rig`,
  `gc slack map-channel`, and `gc slack enable-room-launch` now leads
  with the SIGHUP path (`pkill -HUP gc-slack-adapter`) and offers
  `gc service restart slack` as the fallback, since SIGHUP avoids the
  startup gap.
- Adapter Go source relocated from `examples/oversight-rig/adapter/`
  to `examples/slack-pack/adapter/` (`gc-28a`). The pack is now
  self-contained for upstream extraction into a separate
  `gascity-packs` repo. No behavioral change; the binary path
  (`examples/slack-pack/adapter/gc-slack-adapter`) is unchanged, so
  the supervised `proxy_process` service picks up the new build at
  next restart with byte-identical functionality.
- Build flow simplified to a single command:
  `cd examples/slack-pack/adapter && go build -o gc-slack-adapter`.

### Security

- Default adapter state under `/tmp/gc-slack-adapter/*` is no longer
  world-readable on shared hosts (`gc-ywe.6`). Concretely: the
  identity registry, handle-alias registry, and inbound file store now
  create directories with mode `0o700` and files with mode `0o600`
  (previously `0o755`/`0o644`). Pre-fix installs are migrated on
  startup by a one-shot tightener that walks the three configured
  store paths and chmods only-if-strictly-looser; setuid, setgid, and
  sticky bits are preserved so operator-customized layouts (e.g.
  setgid for shared-group access) survive intact. Operators who
  deliberately set tighter perms (e.g. `0o400` read-only) are also
  left alone. As defense-in-depth, the proxy_process Unix domain
  socket is chmod'd to `0o600` after bind on top of its
  `0o700` controller-managed parent directory at
  `/tmp/gcsvc-<uid>/<hash>/`.

## [0.1.0] - 2026-05-03

Initial preview. Feature-by-feature port of the upstream `discord` pack
shape; today's surface is enough to run a multi-session oversight loop
end-to-end (DMs, rooms, peer fanout, identity overrides, bidirectional
file attachments).

### Added

- `gc slack bind-dm` — bind a Slack DM channel to one named session.
- `gc slack bind-room` — bind a room to multiple sessions, with
  `--enable-peer-fanout`, `--allow-untargeted-publication`,
  `--max-peer-triggered-publishes`, `--max-total-peer-deliveries`,
  `--default-handle`, `--handle HANDLE=SESSION`, and
  `--binding-owner`.
- `gc slack reply-current` — reply to the latest Slack event in the
  current session, routed through gc's `/extmsg/outbound` so transcript
  recording and peer fanout fire (`--via adapter` keeps the direct path
  for diagnostics).
- `gc slack publish` — publish to a session's saved binding (target
  session required, no event-scan fallback).
- `gc slack publish-to-channel` — publish to an arbitrary channel ID
  with no session binding required.
- `gc slack status` — read-only diagnostics over adapters, bindings,
  and recent traffic. Supports `--session SID`, `--since`, and
  `--json`.
- `gc slack react` — add an emoji reaction to a Slack message.
- `gc slack identity` — register and unregister per-session
  `chat:write.customize` identities so each bound session posts under
  its own persona.
- `gc slack handle-alias` — register and unregister cross-channel
  `@handle` to session-id aliases used by the address-by-handle
  protocol.
- `gc slack upload` — bidirectional file attachments
  (`/publish-file` outbound, auto-download of inbound files into
  `$INBOUND_FILE_STORE/<channel>/<ts>-<filename>`, scrubbed by an
  in-process retention janitor).
- `template-fragments/slack-v0.template.md` — composable prompt
  fragment for any agent in a slack-bound session.
- Pack-owned intake service (`[[service]]` proxy_process) supervising
  the adapter via UDS for `/publish`, with the public Slack webhook
  still terminating at adapter TCP `:8775`.
- Native `SessionID` field on `PublishRequest` (replacing the prior
  metadata workaround).
- Scope banner and host-agnostic README copy for upstream-prep
  readiness.
- Adapter env contract documented in the package docstring and in the
  pack README, categorized as must-set / optional-override /
  controller-injected / consumer-specific.

### Changed

- **Breaking (standalone deployments only):** `GC_CITY_NAME` is now
  required. The adapter previously fell back to a hardcoded city name
  when the env var was unset, silently routing inbound traffic to the
  wrong destination. Any standalone (`run.sh`-style) deployment must
  set `GC_CITY_NAME` explicitly. `proxy_process`-supervised deployments
  are unaffected as long as the env file sourced before `gc start`
  defines it.

### Provenance

This release was developed in-tree at `examples/slack-pack/` (with the
adapter at `examples/oversight-rig/adapter/`). Key gascity commits:

- `cfd6d7de` — initial Slack extmsg adapter (Path B).
- `8495e4d7` — pack scaffold + `bind-dm` + `reply-current`.
- `4aa07108` — `bind-room` with peer-fanout policy plumbing.
- `39d92543` — route `reply-current` through `gc /extmsg/outbound`.
- `c1e1f6a1` — adapter UDS mode + `[[service]]` proxy_process
  (`gc-5rz` Phase A).
- `111641dd` — `gc slack status` read-only diagnostics.
- `3edeb3d0` — `gc slack publish` to session bindings.
- `b8abb72d` — identity DELETE + bidirectional file attachments + new
  commands.
- `bfd64511` — native `SessionID` in `PublishRequest`, drop metadata
  workaround (`gc-kvt`).
- `010bc588` — strip host-specific references and add scope banner
  (`gc-ywe.1`, `gc-ywe.4`).
- `3db27544` — document adapter env contract and remove the
  `ds-research` `GC_CITY_NAME` fallback (`gc-ywe.2`).

[0.1.0]: https://github.com/gastownhall/gascity/commits/main/examples/slack-pack

# Changelog

All notable changes to slack-full (formerly slack-pack) are documented in
this file.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

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

# Changelog

All notable changes to slack-full (formerly slack-pack) are documented in
this file.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

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

### Added

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

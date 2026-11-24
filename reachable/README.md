# Reachable

Give the mayor one standing rule: answer a person's message first, in the
conversation where it arrived. Send a job expected to take more than about
30 seconds to a background subagent, or to a worker as a bead for longer work.
The mayor checks the result and reports it in the original conversation.

Importing this pack installs no hook, starts no service, and changes no mode.
It needs Node.js 20 or newer and no packages to install. Nothing leaves the
city: this pack opens no socket, calls no API, and sends no message.

## Give the mayor the rule

Import the pack using your city's normal import configuration. In the city's
configuration, use a patch for one agent:

```toml
[[patches.agent]]
name = "mayor"
inject_fragments_append = ["reachable"]
```

Or append it to all agents' prompts:

```toml
[agent_defaults]
append_fragments = ["reachable"]
```

Keep any existing entries in that list. The prompt must be a `.template.md`
template so GC renders fragments. The hook still acts only when `GC_AGENT`
equals `mayor`, or the explicitly configured `REACHABLE_AGENT`. Subagent calls
identified by Claude's `agent_id` exit before reading any file. Other workers
do too; `agent_type` by itself does not identify a subagent.

## Install and choose a mode

```sh
gc reachable install
gc reachable status
gc reachable mode observe
gc reachable mode enforce
gc reachable mode off
gc reachable uninstall
```

Use your import alias in place of `reachable` if it differs. First install
starts in **observe**. It measures and logs `would_refuse`, but refuses nothing.
**Enforce** makes refusals active. **Off** stops hook measurement. Reinstall
updates the copy after a pack roll and preserves a mode you already chose.

Install copies the script into `.gc/reachable/` and adds unique matcher entries
for PreToolUse, PostToolUse, and Stop to the city's `.claude/settings.json`.
It records the absolute Node executable and city path. It preserves existing
entries, backs up changed settings, and parses the written settings back.
An unchanged second install writes no settings backup. Uninstall removes
only its entries, preserves later additions, backs up its change, and sets off.
When the remaining settings match their original value, uninstall restores the
settings bytes and permissions from first install. It also restores an absent
file and removes an empty directory it created. Later edits retain their value
and current file mode. Uninstall reports when it cannot restore the layout.
The three newest dated backups keep the exact bytes. Uninstall removes the
separate first-install copy.
It never edits the user's settings or GC's generated `.gc/settings.json`.
GC projects the city's settings when it builds a session's runtime settings;
existing sessions may need their normal settings refresh or replacement.

The supported Claude versions, 2.1.309 and 2.1.322, retain hook settings source
in their deduplication key. A city entry also projected through `--settings`
can therefore run twice. Tool-use receipts and turn identities make repeat
invocations idempotent; parallel invocations serialize state or fail open.

## What waits

The hook reads the city's `.gc/events.jsonl`, following its numbered gzip
archives after rotation. A routed `extmsg.inbound` starts a wait. It matches
the configured agent name first, so a replacement mayor inherits unanswered
messages; legacy session-only rows match the current GC session. Duplicate
inbound events do nothing. Named-session wake events and routed inbound rows
identify prior mayor sessions for delivered outbound replies.

Exactly four things stop a wait:

- An `extmsg.outbound` from the mayor's session in that provider/conversation.
- `gc reachable pass PROVIDER/CONVERSATION`.
- The mayor's Stop hook, when its turn ends.
- The message reaching 24 hours old.

Room messages cannot be filtered by addressing from these event rows: they
carry neither conversation kind nor explicit target. GC keeps those fields in
its transcript store, which provides no single bounded local file read for
this decision. Exempt busy conversations explicitly:

```sh
gc reachable exempt example/room-example --on
gc reachable exempt example/room-example --off
gc reachable exempt --list
gc reachable pass example/room-example
```

Pass and new exemptions require a conversation recently routed to the mayor.
You can always remove a listed exemption, including after its routing record
expires. After a pack update, run install if status shows an outdated hook
copy. Until then, status and the exemption list can be read, and changes to
waits and exemptions return `hook_copy_outdated_run_install`.
A Slack reply in a mention-only room can bypass GC's outbound route;
a reaction also leaves no outbound row. After those, pass the conversation by
hand when no further words are needed. An adapter's acknowledgement reaction
does not clear a wait. Direct sends outside GC have the same limitation.

## What the check does

Before a work tool call, enforce mode refuses when someone has waited more
than 30 seconds. It names the conversations, oldest first, and asks for one
line saying what is happening and when the person will hear more. It never
refuses a literal channel-pack command, this pack's own commands, `gc handoff`,
or Claude's own read-only tools. Run install again after renaming an import.
A pipeline, command substitution, redirect or additional work command counts
as work even if the first command talks to a person.

Each waiting message can cause at most three counted work refusals. A refused
attempt beginning with a talk alias or handoff leaves that count intact.
After the third counted refusal the hook lets the message go and records
`let_go`. Observe mode applies the same accounting
with `would_refuse` records. The hook fails open on measurement errors, corrupt
state, a backwards clock, state contention, or a measurement over two seconds.
It suppresses refusals until initial event-log catch-up completes. The cursor
then reads only appended bytes; a partial final row waits for the next call.
The hook skips and counts an unusable complete row. A damaged outbound row
releases earlier waits it could have answered. A row whose type is unreadable
releases earlier waits across conversations. A subsequent valid inbound can
still start a wait. An unexplained sequence gap fails the measurement open.
Archives replay in bounded decoded slices, including the window before a
rotated archive is published. Stop records a sequence fence so unread older
messages stay seen. The registered five-second harness timeout leaves cleanup
time after the internal two-second measurement deadline.
Cold replay also reads old archives for session identities. Old messages do
not become waits. Progress is saved between archives, and enforcement waits
until catch-up completes; a city with a long history can need several calls.
Stop uses the harness prompt ID, or a bounded read of its transcript to obtain
the last user-row UUID. It keeps only the hashed identity, so two tool-free
turns with the same answer remain distinct without storing their text.

PostToolUse only records calls longer than 30 seconds: tool name, duration,
and whether someone waited at the start or arrived during the call. It never
records the command or refuses a call because it ran long. A hook cannot
interrupt a tool that is already running.

## Inspect and test

A quoted reply can contain punctuation, multiple lines and ordinary dollar
amounts or variables. One `cd DIR &&` may precede a talk command. A Write can
prepare a reply body of up to 64 KiB under the system temporary directory or
`/tmp`. The refusal ends with “Refused because” and a short explanation.
The suggested pass command uses this pack's import alias.

`gc reachable status --json` reports mode, registered/current hook state,
catch-up progress, waiting provider/conversation IDs and seconds, and the last
14 days of refusals, let-go cases, and long calls. Plain status prints the
same information. The doctor checks registration in enforce mode, copy
freshness, event-log readability, and the rendered mayor prompt via local
`gc prime --strict`. Neither local configuration command uses a city API.

All state is in `.gc/reachable/`: mode, the installed script and its manifest,
the atomic cursor/waiting/exemption transaction in `state.json`, a derived
`log.jsonl`, and unique `error-*.jsonl` records. Files are 0600 in a 0700 directory and replaced
using a temporary file and rename. Message text and actor names are discarded;
session and tool-use identities are hashed. Settings backups remain here too.

Status shows `skipped_rows` with the count, sequence, reason and time of the
last skip. It also shows error counts and newest times by code, `hook_ok_at`
for the last successful hook save, and `history_dropped` for log entries
evicted by the count limit. Doctor reports recent skips and
`hook_failing_open` when the latest error follows the last successful save.
It prints the error counts even when the state file cannot be read.

Error records retain the newest 100 within 14 days. Duplicate receipts expire
after 10 minutes and retain at most 1,000 entries. Pending tool calls expire
after 24 hours and retain at most 1,000. The 14-day log retains up to 2,000
entries. Session identities and known conversations each retain the newest
1,024 within 14 days. Replay uses the newest event time for each identity and
breaks ties in sequence order. A running hook timestamps its own session with
the current time. Hand-set exemptions persist. Duplicate hook processes
wait for the earlier ticket until 250 ms before the deadline. Lock files more
than ten seconds from the current clock expire. A writer checks its ticket
and the generation of the state it loaded before publishing. The check and
filesystem rename are separate operations, so a successor also withdraws the
expired writer's staged file to cover a pause between them.

From `reachable/test`, run `node --test --test-reporter=tap`. The fixtures use
invented IDs and temporary cities, never a live city's event log. The suite
measures complete-process CPU time by alternating 20 hooks with 20 bare Node
processes. Their median CPU difference must stay below 100 ms. An injected
150 ms of work verifies the check detects a regression. Wall-clock p95 is
reported for both groups. The default suite also measures warm decisions with
full state collections. Use `make test-reachable-perf` for the separate 256 MiB
wall-clock benchmark on a quiet machine. `make test-reachable` runs the default
suite locally and in CI. The root `make test` also runs the Python suites.

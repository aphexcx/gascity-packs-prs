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

Pass and exemption changes reject conversations never observed as routed to
the mayor. A Slack reply in a mention-only room can bypass GC's outbound route;
a reaction also leaves no outbound row. After those, pass the conversation by
hand when no further words are needed. An adapter's acknowledgement reaction
does not clear a wait. Direct sends outside GC have the same limitation.

## What the check does

Before a work tool call, enforce mode refuses when someone has waited more
than 30 seconds. It names the conversations, oldest first, and asks for one
line saying what is happening and when the person will hear more. It never
refuses a literal channel-pack command, this pack's own commands, `gc handoff`,
or Claude's own read-only tools. Import aliases are resolved at install; run
install again after renaming an import. A shell pipeline, expansion, redirect,
or second command is work, even if the first command talks to a person.

Each waiting message can cause at most three refusals. After the third the
hook lets it go and records `let_go`. Observe mode applies the same accounting
with `would_refuse` records. The hook fails open on measurement errors, corrupt
state, a backwards clock, state contention, or a measurement over two seconds.
It suppresses refusals until initial event-log catch-up completes. The cursor
then reads only appended bytes; a partial final row waits for the next call.
Archives replay in bounded decoded slices, including the window before a
rotated archive is published. Stop records a sequence fence so unread older
messages stay seen. The registered five-second harness timeout leaves cleanup
time after the internal two-second measurement deadline.

PostToolUse only records calls longer than 30 seconds: tool name, duration,
and whether someone waited at the start or arrived during the call. It never
records the command or refuses a call because it ran long. A hook cannot
interrupt a tool that is already running.

## Inspect and test

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

From `reachable/test`, run `node --test --test-reporter=tap`. The fixtures use
invented IDs and temporary cities, never a live city's event log. The suite
includes a 256 MiB log, incremental and complete-process p95 measurements,
rotation, duplicate hooks, installation and removal. From the repository root,
`make test` includes this suite and the repository's Python suites.

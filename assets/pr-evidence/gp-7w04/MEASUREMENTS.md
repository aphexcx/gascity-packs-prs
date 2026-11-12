# Reachable measurements

Base: `e84b685a177b109877c4d47d36953e4ad85539ed` of the packs repository.
GC source checked at `a8217556`. All executable probes used synthetic values;
no live hook was installed and no messaging API was called.

## 3: identity across a handoff

`internal/extmsg/inbound.go:111` and `:116` preserve the binding's agent name;
`:141` emits it as `payload.target_agent`, alongside `target_session` when
available. The agent-bound default route can have an empty session field.
`cmd/gc/build_desired_state.go:1186` sets the named session's `GC_AGENT` to its
configured identity. The hook therefore prefers target_agent, with current
session matching only for legacy rows that omit it. Fixture tests exercise a
replacement session, a legacy current session, another agent, and duplicates.

`cmd/gc/session_lifecycle_parallel.go:2664` writes a named session's wake with
its display identity in subject and its session bead in session_id. The hook
uses these rows and routed inbound rows to recognize delivered replies from
prior mayor sessions. A fixture verifies an old-session reply after handoff.

## 6: subagent identity

The Linux native package for Claude Code 2.1.309 and the installed macOS
2.1.322 binary both carry the same base hook field semantics: agent_id is
present inside a subagent and absent on the main thread, including a main
thread started with an agent type. The PreToolUse constructor spreads that
base input into its payload.

`native-probes.json` records binary SHA-256, the base builder's symbol and byte
offset, and the outputs of isolated executions of those builders. Only
unrelated context helpers were stubbed. Both versions produced:

| Input context | agent_id | agent_type |
| --- | --- | --- |
| Main thread | absent | absent |
| Subagent | subagent-example | general-purpose |
| Main thread with an agent type | absent | reviewer-example |

No model session was started. The pack tests use those fixture shapes and
assert that a subagent bypasses all state handling.

## 9: tools and imported names

The native bundles contain the exact harness tool names used by the single
policy table: Read, Glob, Grep, WebFetch, WebSearch, ListMcpResources,
ReadMcpResource, ToolSearch, TaskGet, TaskList, and TaskOutput. The table has an
individual fixture test for each tool. Unknown tools remain work calls.

`cmd/gc/cmd_import.go:910` implements the local `gc import list` read. Its
tab-separated rows begin with the import alias, followed by source. Direct
aliases are executable; `(transitive)` is not an alias. The pack resolves the
four allowed pack identities to those aliases at install. Fixtures cover
renamed aliases and a local directory whose name differs from its pack name.
Remote imports use the installed `packs.lock` commit and GC's source/commit
cache to inspect the actual manifest, including GitHub blob/tree, Git, SSH,
file and repository-root forms. Unresolved bundled non-channel imports grant
no exemption and do not prevent resolution of the channel packs.

The literal shell parser permits only one command. Its fixtures reject
pipelines, redirects, substitutions, chained commands, and unquoted shell
operators while allowing punctuation inside a quoted message argument.

## 5: busy rooms

`internal/extmsg/transcript_service.go:95` stores conversation_kind and
explicit_target in transcript bead metadata. The inbound event itself does
not carry those fields or a transcript entry identifier. There is no single
bounded local file read from this event to that metadata under the pack's
no-API constraint. Automatic room-address filtering is therefore omitted;
the explicit exemption list is implemented and tested.

## 16: duplicate settings entries

The native hook deduplication key in both versions includes settingsSource.
The isolated native-key probe keeps two keys for the same command supplied
through projectSettings and flagSettings. Thus the city's own entry and GC's
projected `--settings` entry can both run: two source-distinct hooks survive
deduplication. This is a source-level measurement, not a live harness launch.
Fixtures exercise repeated PreToolUse, PostToolUse, and Stop inputs; no live
city settings were touched.

## 13: large-log timing

The suite constructs an exact 256 MiB event file and advances its cursor to
the end. It then measures 100 warm decisions and 20 complete Node hook
processes. Both p95 values must remain below 100 ms. See the green transcript
for this head's measurements. Initial catch-up is intentionally separate from
the incremental timing and never refuses while a later reply remains unread.

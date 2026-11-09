Bind a Slack room/channel (public, private, or multi-party DM) to one or
more named sessions, optionally creating a conversation group with a
peer-fanout policy and per-session participant handles.

Each session bound to the room receives an inbound system-reminder when
a human posts in the channel. When peers publish through the gc
outbound API, every other bound session is also notified — that's how
mayor and project-leads end up visible to each other inside one
conversation while a human watches.

Delivery modes
--------------

AMBIENT (default): the session becomes a gc participant of the room's
conversation group and gc wakes it for EVERY inbound message.

MENTION-ONLY (`--mentions-only`): the session is registered with the
slack adapter instead (POST /mention-only), never as a gc participant. The adapter
injects a `Slack mention-only room delivery` reminder into the session
only when the message

  (a) @mentions the adapter's bot user (or arrives as an app_mention),
      or addresses the session's handle — `@handle: …`, a Slack User
      Group mention that maps to it, or a thread-sticky handle; or
  (b) is a reply in a thread whose root or an earlier reply was posted
      by that session through this adapter (`gc slack reply-current`,
      `publish-to-channel`, `upload`), so follow-ups to its own posts
      still arrive.

Everything else in the room is dropped for that session — no wake, no
reminder — but stays readable on demand with
`gc slack read --conversation-id <C…>`. Ambient participants bound in a
separate invocation are unaffected, and the room's channel copy still
reaches gc exactly as before. The `@handle:` alias dispatcher keeps
working; a message it already injects into the same session is not
delivered twice. The reminder prescribes the bindingless `gc slack publish-to-channel
--conversation-id <C…> --thread-ts <root>` reply; `reply-current
--thread-current`, `react`, and `upload --thread-current` also resolve
mention-only deliveries on their own (via the adapter's delivery log)
and publish through the adapter, since the session holds no gc binding
for the room. A mention-only delivery newer than the session's
company-room/DM current-turn pointer wins over the company dispatch in
`reply-current`; `--turn-ref`, `--origin-ts` or `--kind room|dm|mpim`
pin the company turn explicitly.

When gc reports no ambient participants or explicit direct binding, the
command holds the room in a zero-participant gc conversation group and
removes any agent binding created by `extmsg-default-route`. This prevents
the city's sticky default route from waking a session on every line: gc
resolves direct bindings first, but never default-routes a grouped room
(`internal/config/extmsg.go` and `internal/extmsg/inbound.go` in gc). Existing
ambient routes are preserved. Live gc records are read through `GET /beads`
filtered by this room's group-root and binding-conversation labels, including
all cursor pages; incomplete reads fail the command. Those labels use gc's
SHA256 of the normalized six-part conversation encoded with Go's JSON rules.
The result reports `group_held` and `unbound_bindings`, and `gc slack status`
shows the live hold state. `--no-group-hold` skips this reconciliation and
registers with the adapter alone; it does not remove an existing hold.

The hold is ensured and read back before inspecting bindings for removal.
An authoritative `GET /extmsg/groups` also checks absence before an ensure:
if it disagrees with the labeled inventory, the command refuses to upsert
and risk resetting an existing group's policy. Both ambient and mention-only
`bind-room` commands take an exclusive flock on
`<city>/.gc/services/slack/data/room-hold.lock`, beside `config.json`, from
their first config read through routing and persistence. Concurrent pack
invocations on the same host serialize. Hand-run `gc extmsg bind`, other
gc callers, and default routing do not take this lock.

Immediately before unbinding, the command compares the room's binding IDs
and metadata with its post-hold inspection. A changed binding set aborts
removal with exit 1 and `hold: partial (binding changed during the hold;
re-run bind-room)`; the group remains in place. The result and config record
name the filter as `unbind_filter: conversation+agent (gc exposes no binding
id; see help)`. A partial result is not proof that ambient delivery stopped.

gc currently exposes only `conversation`, `session_id`, and `agent_name` on
POST `/extmsg/unbind` (`internal/api/huma_types_extmsg.go:98–105` at
`92a7f383ff69a2b9a995239a6612fe0735c6f09d`). GET `/extmsg/bindings` accepts
only `session_id` (lines 67–71), hence the labeled-bead inventory. There is
no binding-ID or expected-caller conditional delete. An explicit same-agent
rebind by an external operator between the final read and unbind can still
be removed, even if the window is only milliseconds. Serialize external
routing edits too. The gc-side follow-up is an atomic binding-identity and
caller-provenance predicate on unbind, plus conversation-filtered binding
reads that include agent bindings and their caller; only that closes the
remaining race. The pack's group prevents new default routing once visible,
but cannot fence a gc inbound that already passed its group check.

To restore ambient routing, use `bind-room ROOM SESSION --group-only` or
`bind-room ROOM SESSION --binding-owner SESSION`. Both add an ambient
participant and clear the recorded hold. If that session is already registered
mention-only, first remove its adapter registration with
`DELETE …/svc/slack/mention-only?channel_id=<ROOM>&session_id=<id>`.
`--group-only` removes direct bindings and clears any previously recorded
binding owner; it does not retain an owner it just unbound.

`--mentions-only` cannot be combined with the peer-fanout flags,
`--default-handle`, or `--binding-owner` — those configure gc group
participants, which a mention-only session is not. Bind ambient and
mention-only sessions for the same room in two invocations.

Imported COMPANY rooms (`gc slack import-company-directory`) work too:
the company gateway owns those channels' events and delivers to the
room's company members itself, and it runs the mention-only lane for
every registered session that is NOT a company member of the room
(same rules: bot @mention, `@handle:` prefix, own-thread follow-ups).
A mention-only binding for a session that IS a company member of the
room is redundant there and is skipped — the gateway's delivery is its
copy, never a second one.

A session already bound AMBIENTLY to the room (gc group participant or
`--binding-owner`) is refused for `--mentions-only`. Once its gc-side
membership has been removed, re-run with `--replace-ambient`: the
conflict check reads the pack config's binding record, and only that
flag drops the stale ambient entry from it. A session gc still reports
as actively bound to the room stays refused.

Examples
--------

Every run must declare who owns the room's direct binding — either
`--binding-owner SESSION` or `--group-only`. See "Binding authority" below.

Group-routed room (every session sees inbound, default-routed to the
first session for explicit-target resolution; no direct binding):

  gc slack bind-room C0123ROOM01 oversight-rig.mayor geo/oversight-rig.project-lead \
      --group-only

Room with an outbound publisher (the shape oversight-rig uses):

  gc slack bind-room C0123ROOM01 \
      oversight-rig.mayor geo/oversight-rig.project-lead \
      --binding-owner gc-77139

Mention-only binding for a busy channel that is mostly someone else's
lane (the session is woken only when addressed or replied to):

  gc slack bind-room C0B2Y13DRMK mayor --mentions-only

Enable peer-fanout policy with caps (governs peer-triggered publishes):

  gc slack bind-room C0123ROOM01 \
      oversight-rig.mayor geo/oversight-rig.project-lead \
      --group-only \
      --enable-peer-fanout \
      --allow-untargeted-publication \
      --max-peer-triggered-publishes 8 \
      --max-total-peer-deliveries 24

Override participant handles (used by `@handle:` routing; with
`--mentions-only` the handle is what `@handle: …` must name to reach
the session):

  gc slack bind-room C0123ROOM01 \
      oversight-rig.mayor geo/oversight-rig.project-lead \
      --group-only \
      --default-handle mayor \
      --handle mayor=oversight-rig.mayor \
      --handle geo-pl=geo/oversight-rig.project-lead

Underlying calls
----------------

Ambient:

1. POST /v0/city/<name>/extmsg/groups   (mode=launcher; with fanout policy if any flag set)
2. POST /v0/city/<name>/extmsg/participants for each session
3. Reconcile gc's authoritative direct-binding table so it cannot shadow the
   group: POST /extmsg/unbind for a `--group-only` room, or POST /extmsg/bind
   with `replace=true` when `--binding-owner` is set.

Binding authority
-----------------

gc resolves an inbound message against a direct binding *before* the group
route, so a stale direct binding silently shadows the group. Step 3 makes the
binding table agree with the topology you declare — and because that step
writes authoritative state, exactly one of these is required on every run:

  --binding-owner SESSION   keep (or hand over) the room's outbound publisher.
                            Any other direct binding for the room is replaced.
  --group-only              declare the room group-routed. Removes EVERY active
                            direct binding for the conversation — including one
                            created by other tooling or another operator.

Passing neither is an error, not a default: `--group-only`'s sweep is
destructive, and an operator who forgets `--binding-owner` on a re-run would
otherwise sever the room's outbound publishing without being told. The removed
bindings are printed to stderr and returned as `unbound_bindings`.

Mention-only:

1. GET  /v0/city/<name>/sessions  (resolve each session name to its gc id)
2. POST /v0/city/<name>/svc/slack/mention-only for each session
   ({channel_id, session_id, session_name, handle})
3. Unless `--no-group-hold`: inspect live groups, participants and bindings
   through GET /beads (also checked before registration). With no ambient
   route, POST /extmsg/groups with mode=launcher and no participants if the
   group is missing, then POST /extmsg/unbind for each default-route agent,
   filtered by both conversation and agent_name. Re-read to verify the hold.
   An existing empty group is reused without changing its policy or cursor.

The pack records the binding under
`.gc/services/slack/data/config.json` so other slack-pack commands can
resolve the room without re-querying gc; a mention-only binding is the
record's `mention_only_participants` list and `group_held` boolean (`delivery_mode:
mentions_only` when the room has no ambient participants). The
adapter's own registry — what it actually enforces — lives at
`<GC_CITY_PATH>/.gc/slack/mention_only_bindings.json`
(`SLACK_MENTION_ONLY_BINDINGS_FILE`) and is listed by `gc slack status`
and by `GET …/svc/slack/mention-only`. To remove a mention-only binding:
`DELETE …/svc/slack/mention-only?channel_id=<C…>&session_id=<id>`.

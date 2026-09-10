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
slack adapter instead (POST /mention-only), never with gc. The adapter
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
delivered twice. `reply-current --thread-current`, `react`, and
`upload --thread-current` resolve mention-only deliveries on their own
(via the adapter's delivery log) and publish through the adapter, since
the session holds no gc binding for the room.

`--mentions-only` cannot be combined with the peer-fanout flags,
`--default-handle`, or `--binding-owner` — those configure gc group
participants, which a mention-only session is not. Bind ambient and
mention-only sessions for the same room in two invocations.

Examples
--------

Plain ambient binding (every session sees inbound, default-routed to
the first session for explicit-target resolution):

  gc slack bind-room C0123ROOM01 oversight-rig.mayor geo/oversight-rig.project-lead

Mention-only binding for a busy channel that is mostly someone else's
lane (the session is woken only when addressed or replied to):

  gc slack bind-room C0B2Y13DRMK mayor --mentions-only

Enable peer-fanout policy with caps (governs peer-triggered publishes):

  gc slack bind-room C0123ROOM01 \
      oversight-rig.mayor geo/oversight-rig.project-lead \
      --enable-peer-fanout \
      --allow-untargeted-publication \
      --max-peer-triggered-publishes 8 \
      --max-total-peer-deliveries 24

Override participant handles (used by `@handle:` routing; with
`--mentions-only` the handle is what `@handle: …` must name to reach
the session):

  gc slack bind-room C0123ROOM01 \
      oversight-rig.mayor geo/oversight-rig.project-lead \
      --default-handle mayor \
      --handle mayor=oversight-rig.mayor \
      --handle geo-pl=geo/oversight-rig.project-lead

Underlying calls
----------------

Ambient:

1. POST /v0/city/<name>/extmsg/groups   (mode=launcher; with fanout policy if any flag set)
2. POST /v0/city/<name>/extmsg/participants for each session

Mention-only:

1. GET  /v0/city/<name>/sessions  (resolve each session name to its gc id)
2. POST /v0/city/<name>/svc/slack/mention-only for each session
   ({channel_id, session_id, session_name, handle})

The pack records the binding under
`.gc/services/slack/data/config.json` so other slack-pack commands can
resolve the room without re-querying gc; a mention-only binding is the
record's `mention_only_participants` list (`delivery_mode:
mentions_only` when the room has no ambient participants). The
adapter's own registry — what it actually enforces — lives at
`<GC_CITY_PATH>/.gc/slack/mention_only_bindings.json`
(`SLACK_MENTION_ONLY_BINDINGS_FILE`) and is listed by `gc slack status`
and by `GET …/svc/slack/mention-only`. To remove a mention-only binding:
`DELETE …/svc/slack/mention-only?channel_id=<C…>&session_id=<id>`.

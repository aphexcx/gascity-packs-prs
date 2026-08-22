# Ops board — to do / doing / done over all beads, with STALLED / WAITING chips

Bead: `gp-6xd` (Taylor ask, Slack C0AMHQ74ZLK ts=1787111488.948379, 2026-08-19 03:51Z).
Status: **SUPERSEDED 2026-08-19 04:10Z — Afik re-scoped: no new /board route or endpoint; use the
existing /beads dashboard with the smallest delta.** See `2026-08-19-ops-board-audit.md` (audit +
delta proposal). Kept for the chip rules, which the delta reuses client-side. The server-side
implementation of this design is parked, unmerged, on `aphexcx/gascity` branch `feat/ops-board`.
Original status line: design draft → mayor check → build. Author: gascity-packs/gc.implementation-worker (session ci-uukrm).

Taylor, verbatim: *"a very very simple task management/list that has 3 stages — to do, doing,
done — a visual that enables us to see what's happening at all times from everyone … primarily
worried about situations where a subagent was tasked to do something and they stalled and it
was somewhat lost until I followed up."*

## 1. What it is (and is not)

- One read-only page (`/board` in the gc dashboard), one API endpoint
  (`GET /v0/city/{cityName}/board`), one CLI (`gc board`). All three render the **same
  server-computed model**; the SPA and CLI do no chip logic of their own.
- **Beads are the only source of truth.** Columns are a 1:1 projection of bead status. No
  second task store, no board-side state, no drag-drop or editing in v1 (state still changes via
  `gc bd` / `bd`).
- The one thing the existing `/beads` page does not do — and the reason this exists — is the
  **status chip**: every card gets exactly one chip computed server-side from *bead ⋈ session*
  (ACTIVE · IDLE · STALLED · WAITING ON <human> · BLOCKED), and STALLED/WAITING cards float to the
  top of *Doing* with a red/amber bar so a lost subagent is visible without anyone following up.

## 2. Mock (Asana-like, but very simple)

```
gas city · Board                                   [rig: all ▾] [owner: all ▾] [search…]  ↻ 42s ago
STALLED 2 · WAITING 2 · BLOCKED 3 · doing 12 · to do 74 · done (48h) 31

TO DO (74)                    DOING (12)                                DONE · last 48h (31)  ▸ collapse
──────────────────────────    ────────────────────────────────────────  ──────────────────────
P1 gp-snd citadel ROLL…       ┃ STALLED 41h  ci-894s                    ✓ gp-i62 reply-current…
   gascity-packs · 3h         ┃ Read assignment, implement, verify…       gascity-packs · 2h ago
                              ┃ city · claude-1 · session idle 41h        by gc__impl…-ci-r3z
P1 hw-vdkin INCIDENT 8/17…    ┃                                         ✓ gp-lie gc slack read…
   houmanoids-www · 1d        ┃ STALLED 2d   ci-5xui                      gascity-packs · 6h ago
                              ┃ VC seed-round fit research: 22 …        ✓ ci-3607z cross-city…
P2 gp-4ou gc hook --claim…    ┃ city · unassigned · no session · hb 14d    city · 9h ago
   gascity-packs · 4h         ┃                                         …
                              ┃ WAITING ON Taylor+Afik 40h  hw-6oqx2
P2 ci-8bjx6 dcg redirect…     ┃ LIVE patrol tile: index tile becomes…
   city · 5h                  ┃ houmanoids-www · impl-worker-5 · pinned
                              ┃
…                             ┃ WAITING ON Afik 6d  ci-5xui   (if checkpoint metadata present)
                              ┃
BLOCKED (3, dimmed, bottom)     ACTIVE 2m   gp-6xd
P2 hw-augjs bead upgrade…       Ops board (Taylor 8/18)…
   houmanoids-www · needs 1     gascity-packs · impl-worker-1 · hb 1m ago
                                IDLE 22m    hw-mma6  Deploy/OTA security…
```

Card = **title · id (link → `/beads?bead=<id>` detail modal) · rig · owner/assignee · age ·
last-activity · ONE chip.** Left bar: red = STALLED, amber = WAITING, grey = BLOCKED, none
otherwise. Chip text carries the duration ("STALLED 41h", "WAITING ON Afik 6d") so the cost of
ignoring it is legible at a glance.

Filters: rig (select), owner/assignee (select), free-text search (id/title/assignee) — all
client-side over the one board payload; the URL carries them (`?rig=…&owner=…&q=…`) so a
filtered board is linkable from Slack. Done column collapsible, default expanded, last 48h.
Auto-refresh every 30s (visible-tab only, same `useVisibleRefresh` hook as the other pages),
plus the SSE `bead.*`/`session.*` refresh the Beads page already uses (10s coalesce).

## 3. Column + chip rules (server-side, deterministic)

Inputs per bead: `status, assignee, created_at, updated_at, metadata (gc.session_name /
gc.session_id / gc.last_heartbeat_at / gc.checkpoint_hold / gc.checkpoint / gc.founder_gate /
gc.kind), dependencies, is_blocked, defer_until, ephemeral, labels`.
Inputs per session (`session.Info` via the sessions read model + runtime overlay):
`id, session_name, alias, state, last_active, rig, template`.

**Noise filter (what is NOT a task).** Excluded from every column: ephemeral/wisp beads
(`ephemeral` or `-wisp-` id), `issue_type ∉ {task, bug, feature, epic, chore}` (drops
message/session/convoy/molecule rows), labels prefixed `gc:` / `extmsg:` / `order-tracking` /
`order-run:` (Slack transcript beads and order runs — ~95% of the raw open set today: 2957 open
→ ~150 tasks), and graph.v2 latch/control beads `gc.kind ∈ {workflow, scope, check, fanout,
scope-check, workflow-finalize}` (worker step beads with no `gc.kind` stay — they are the real
work). Deferred beads (`defer_until` in the future) leave *To do*.

**Columns.**
- **To do** = `open`, not deferred. BLOCKED chip when `is_blocked==true`, or (projection
  absent) any `blocks` dependency targets a bead that is still open/in_progress in the fetched
  set. Blocked cards sort last and render dimmed — they stay visible so nothing is "lost", but
  they don't compete with ready work.
- **Doing** = `in_progress`.
- **Done** = `closed` with `updated_at` (bd has no `closed_at` on the wire) within `done_hours`
  (default 48). Excluded from the fetch entirely if the client asks `done_hours=0`.

**Session join.** A card's session is resolved in this order (same precedence the runs view
uses): `metadata.gc.session_id` → session id; `metadata.gc.session_name` → `session_name`;
`assignee` matched against every identity `session.AssigneeIdentities()` yields (bead id,
session_name, named identity, alias, prior aliases). Result: `session = {id, name, state,
last_active}` or null.

**last_activity_at** = max(`updated_at`, `metadata.gc.last_heartbeat_at`, `session.last_active`
if joined). `idle = now − last_activity_at`.

**Chip precedence (first match wins) — Doing column:**

| chip | rule | bar |
|---|---|---|
| `WAITING ON <who>` | bead carries `gc.checkpoint_hold`, `gc.checkpoint=awaiting-human`, or `gc.founder_gate`, or a label `hold:*` (bd's mayor/external holds). `who` parsed from the metadata text when it names Taylor/Afik/mayor, else "human". Duration = since the hold metadata's `updated_at` (we can't do better without a stamp; Phase 2 adds `gc.checkpoint_since`). | amber |
| `STALLED <for>` | `in_progress` AND (no session resolved, or session.state ∈ {drained, draining, archived, failed-create, quarantined, suspended, asleep}, or `idle > 60m`). `reason` field says which: `no session` / `session drained` / `idle 41h`. Duration = idle. | red |
| `IDLE <for>` | session alive, `15m < idle ≤ 60m`. | — |
| `ACTIVE` | session alive, `idle ≤ 15m`. | — |

To do: `BLOCKED (needs N)` or `READY` (no chip text; the column is the state). Done: `DONE
<ago>`.

Note on leases: bd's `lease_expires_at` is not on the gc wire (bd-native, off `beads.Bead`), and
in practice leases only renew when a worker runs `gc bd heartbeat`, so most healthy sessions
show an expired lease within 5 min of claiming (this bead did). Lease expiry is therefore **not**
a stall signal in v1; `gc.last_heartbeat_at` (which `gc bd heartbeat` stamps) feeds
`last_activity_at` instead. If bd later exposes leases on the wire we add `lease_expired:true`
as a secondary STALLED reason.

Thresholds (15m / 60m / 48h) are constants in one place server-side and appear in the response
(`thresholds`), so the SPA legend and the CLI print the same numbers.

**Sort.** Doing: STALLED (longest idle first) → WAITING (longest first) → IDLE → ACTIVE, then
priority, then oldest first. To do: unblocked by priority then age; blocked last. Done: most
recently updated first.

## 4. Endpoint

`GET /v0/city/{cityName}/board` — Huma op (required by the non-Huma surface guard), read-only,
same read-auth posture as every other `/v0/city/{cityName}` GET.

Query: `rig` (repeatable; default all rigs + city), `assignee`, `q` (id/title/assignee
substring), `done_hours` (default 48, 0 = skip closed), `include_noise=1` (debug: disable the
noise filter). Filters run server-side too so the CLI/Slack digest can ask for a slice.

Response (`BoardBody`):

```jsonc
{
  "city": "city",
  "generated_at": "2026-08-19T04:10:00Z",
  "thresholds": { "active_s": 900, "stalled_s": 3600, "done_hours": 48 },
  "counts": { "todo": 74, "doing": 12, "done": 31, "stalled": 2, "waiting": 2, "blocked": 3 },
  "rigs": ["city", "gascity-packs", "houmanoids-www"],
  "columns": {
    "todo":  { "count": 74, "cards": [ …Card ] },
    "doing": { "count": 12, "cards": [ …Card ] },
    "done":  { "count": 31, "cards": [ …Card ] }
  },
  "partial": false, "partial_errors": []
}
// Card
{
  "id": "hw-6oqx2", "title": "LIVE patrol tile…", "rig": "houmanoids-www",
  "status": "in_progress", "issue_type": "task", "priority": 1,
  "assignee": "gc__implementation-worker-ci-hbbji", "owner": "mayor",   // owner = created_by/from
  "created_at": "…", "updated_at": "…", "last_activity_at": "…",
  "age_s": 147000, "idle_s": 1200,
  "chip": { "kind": "waiting", "label": "WAITING ON Taylor+Afik", "who": "Taylor+Afik",
            "since": "2026-08-17T13:08:00Z", "duration_s": 144000,
            "reason": "gc.checkpoint_hold: founder design review (Taylor+Afik)" },
  "session": { "id": "ci-hbbji", "name": "gc__implementation-worker-ci-hbbji",
               "state": "active", "last_active": "…" },     // or null
  "blocked_by": ["hw-xxxx"],                                    // open deps, To do only
  "labels": ["…"], "url": "/beads?bead=hw-6oqx2"
}
```

`chip.kind ∈ {active, idle, stalled, waiting, blocked, ready, done}` is a closed enum (Huma
enum) so clients switch on it; `label` is display text.

**Multi-city (follow-up bead, not built now).** The body is self-describing (`city` on the
envelope, `rig` on every card, `generated_at`), so a client that pulls `/board` from N cities via
`gc --context <peer>` merges by concatenating columns and re-sorting with the same rules; the
CLI grows `--context a,b` then. Nothing in v1 assumes a single city beyond the path param.

**Federation + caching.** Same leg order as `beads/ready`: city store → rigs ascending → graph
store; per-leg failure = `partial:true` + `partial_errors`. Two `ListQuery` legs per store
(`open`+`in_progress` in one via `Live`, and `closed` with an updated-since bound). Response is
time-bucketed in the existing response cache like `/status` (5s bucket) — the board is O(all
rigs) and the SPA polls.

**Auth today (asked in the bead).** The supervisor listener is loopback `:8372` and the tailnet
gets it via `tailscale serve :8443 → 127.0.0.1:8372` (`allowed_hosts` includes the ts
hostname). Reads are ungated unless `ReadAuthVerifyKey` is set (it isn't) — so `/board`, like
`/beads` and `/sessions`, is readable by any tailnet peer and NOT reachable from the public
funnel. Writes are CSRF/write-auth gated and the SPA flips to read-only when the backend says
so; the board has no write path at all. The pending hardened `:8443` listener from
gp-snd/ci-n6iis keeps GET open, so nothing changes for Taylor/Afik: `https://<ts-host>:8443/board`.

## 5. CLI

`gc board [--rig R]... [--assignee A] [--q TEXT] [--done-hours 48] [--json] [--only
stalled,waiting]` → hits the endpoint via the city-scoped API client (`route=api`), prints
three sections in the same order/sort with the chip first on each line, e.g.

```
DOING (12) — STALLED 2 · WAITING 2
  STALLED 41h        ci-894s   Read assignment, implement, verify…      city · claude-1
  WAITING Taylor+Afik 40h  hw-6oqx2  LIVE patrol tile: …              houmanoids-www · impl-worker-5
  ACTIVE             gp-6xd    Ops board (Taylor 8/18)…                gascity-packs · impl-worker-1
TO DO (74) — BLOCKED 3
  P1  gp-snd …
DONE last 48h (31)
```

`--json` dumps the body verbatim (mayor's Slack digest = `gc board --json | jq` or
`gc board --only stalled,waiting`). No API fallback path: if the supervisor is down the command
says so (the board is about live sessions; there is nothing meaningful to compute offline).

## 6. Dashboard route

- `frontend/src/routes/Board.tsx` + `components/board/{BoardColumn,BoardCard,ChipBadge}.tsx`,
  fetching `/v0/city/{city}/board` through the generated supervisor client (`npm run
  generate:client` after the spec change) — no BFF, matching every other page.
- Nav: `EXPLICIT_ROUTES` entry `{ to: '/board', label: 'Board', order: 25 }` (between Agents
  and Beads). Board also becomes eligible as `DEFAULT_VIEW=board`.
- Design register: the dashboard's DESIGN.md is greyscale/editorial ("never a colored left
  edge"). The mayor explicitly asked for a red/amber left bar for STALLED/WAITING; those are
  the same two anomaly tones the attention system already uses (`attentionListItemProps` →
  `border-accent` / `border-warn`), so the board reuses those classes rather than inventing
  colors — the bar is an *attention* mark, which DESIGN.md sanctions. Flagged in the upstream PR
  description.
- Card click opens the existing `BeadDetailModal` (`?bead=<id>`), so the board needs no
  detail view of its own.
- No writes, no drag-drop; the read-only banner logic is untouched.
- Tests: chip/sort logic is Go (table tests on synthetic beads+sessions, incl. the four live
  cases above); SPA gets a render test with a fixture body; Playwright screenshot against the
  local supervisor for the mayor mail. `make dashboard-build` regenerates `dist/` (committed).

## 7. Phase 2 (separate bead, filed at close, not built now)

Stall/waiting alerts to Slack: a small `gc board watch` loop (or the board-watcher pack) polls
`/board` every N minutes, keeps last-seen `chip.kind` per card, and posts ONE deduped message
per transition into STALLED, or WAITING >2h, with the owner mention and the card link; clears
with a "recovered" reply in-thread. Needs `gc.checkpoint_since` stamped by whoever sets a hold
so WAITING durations are exact. Also: `lease_expired` as a stall reason once bd exposes leases
on the wire; multi-city merge via `--context`.

## 8. Build plan (after mayor check)

1. Worktree off `aphexcx/gascity` `integration`: `internal/api/huma_types_board.go`,
   `huma_handlers_board.go` (+ `board_model.go` pure functions + tests), route in
   `supervisor_city_routes.go`, `go run ./cmd/genspec && go generate ./internal/api/genclient`,
   `client.go` `GetBoard`, `cmd/gc/cmd_board.go` + `main.go` + `go run ./cmd/genschema`.
2. SPA: generate client, route/components/test, `make dashboard-build`, commit `dist/`.
3. `make build`, run against the live city, Playwright screenshot of `/board`.
4. codex review STANDARD, DRAFT PR on the fork + upstream PR (gastownhall/gascity), mail
   mayor with screenshots; file the Phase-2 bead + the multi-city follow-up.

## Open questions for the mayor (answer inline or in mail; defaults in **bold**)

1. Blocked open beads: **shown at the bottom of To do, dimmed, BLOCKED chip** vs. hidden. The
   bead text says "To do = open (not blocked/deferred)"; I read that as "not competing with
   ready work", not "invisible" — invisible is how things get lost.
2. Noise filter: the label/type/kind exclusions above (drops Slack transcript beads, order
   runs, wisps, workflow latches, convoys). Anything else that should count as a task? Convoys
   as cards: **no** (their step beads are the cards).
3. STALLED idle threshold **60m** and ACTIVE **15m** as the mayor specified; WAITING duration
   from `updated_at` until a `gc.checkpoint_since` stamp exists — OK for v1?
4. Done window **48h** default, collapsible; `?done_hours=` for more.

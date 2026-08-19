# gp-6xd audit — what the existing gc dashboard already gives Taylor, and the smallest delta

Re-scope per Afik (04:08Z): "just show one of the available bead dashboards … don't reinvent
this wheel." This is the audit of the existing SPA (gc 1.4.2, build 559c00e14, supervisor
127.0.0.1:8372, also live on the tailnet at https://tailor512g8s-mac-studio.magpie-egret.ts.net:8443
via the hardened :8447 listener — GET open, verified 200 for /city/city/beads and /v0/city/city/status
from this host), Playwright-headless screenshots in this folder, and a delta proposal.
Nothing has been built for the delta; the /board scaffolding stays parked on branch
`feat/ops-board` in ~/code/gascity-wt-opsboard (3 commits, not PR'd).

## Screenshots (1440×900, light theme, full page)

| file | page |
|---|---|
| `beads-top.png` | /city/city/beads — header, truncation banner, NEEDS YOU panel |
| `beads.png` | /city/city/beads full page (9.5K px tall) |
| `beads-board-ci.png` `beads-board-gp.png` `beads-board-hw.png` | the per-rig kanban sections |
| `beads-detail-hw-6oqx2.png` `beads-detail-gp-6xd.png` `beads-detail-ci-894s.png` | bead detail modal (?bead=id) |
| `agents-top.png` `agents.png` | /city/city/agents |
| `runs.png` `mail.png` `activity.png` `health.png` | the other pages |
| `*.txt` | innerText dumps of each page for grep |

## 1. What /beads shows today (the closest thing to Taylor's board)

Route: `/city/{city}/beads`. Nav badge "Beads 47" = the NEEDS YOU count.

- **Header**: "61 open, 2 in progress." + buttons OPEN WORK · NEW BEAD · REFRESH.
- **Truncation banner** (amber): "Fetch window covered 1000 of 2985 store beads. Raise the fetch
  limit (currently 1000) if engineering work sits past the window." — see finding F1.
- **NEEDS YOU (47)** panel: attention rows. Only two reasons exist today
  (`src/attention/beadsNeedingAttention.ts`): `ready-unclaimed` (open, no assignee, >24h → watch,
  >72h → attention) and `escalated` (open `gc:escalation` bead). Every row = id + reason chip +
  title + "opened Nh ago" + status.
- **Controls**: search box ("Search beads": id/title/assignee/labels), status chips
  **open / in progress / blocked / closed** (client-side; `closed` also widens the fetch), **Rig
  filter** select (all / ci / gp / hw), sort toggle.
- **Kanban, one section per rig** (ci, gp, hw), columns **READY · OPEN · IN PROGRESS · BLOCKED ·
  DONE** (`lib/beadGraph.ts` BOARD_COLUMNS: ready = open with no unresolved deps, open = has
  unresolved deps, blocked = bd status blocked, done = closed — only when the `closed` chip is on).
  Each card: title (2 lines) · id · P<n> · "needs N / blocks M" · "unresolved". **No assignee, no
  age, no last-activity, no session state on the card.** Card click → detail modal.
- **Detail modal**: title, ID·TYPE·P, STATUS, TYPE, ASSIGNEE, CREATED, DESCRIPTION, DEPENDENCIES,
  RELATED (session links "as of Ns"), CLOSE button. Metadata (gc.checkpoint_hold, gc.founder_gate,
  gc.last_heartbeat_at, gc.session_id) is **not rendered anywhere**.
- **Data path**: one `GET /v0/city/{city}/beads?limit=1000` (created_at DESC), client filters to
  issue_type ∈ {feature,bug,task,epic,chore,decision} and drops `gc:*`-labelled rows; sessions
  are ALSO fetched (`GET …/sessions`, used only for the modal's related-session links). Refresh:
  SSE bead.* events coalesced to 10 s + manual REFRESH.

So Taylor's need (a) — a to-do / doing / done grouping — **exists** (READY+OPEN / IN PROGRESS /
DONE per rig, plus status chips). Need (b) — a stalled/waiting cue — **does not exist on any
page**, and finding F1 makes (a) actively misleading today.

## 2. What the other pages show

- **/agents** (`agents-top.png`): "5 idle, 1 rate-limited." · NEEDS YOU (rate-limited agent) ·
  **Workers active** (session-driven: `gascity-packs · gc.implementation-worker · active · 5s`,
  `houmanoids-www · … · 40m`, PEEK) · Available agents table: Agent, State (**waiting** ▲ for
  claude-ci-e47j = has an active bead but no runtime activity for >10 min — `computeAgentState`),
  Activity, Context, Last active. This is the ONLY liveness cue in the SPA and it is per
  agent/session, not per bead; it does not say WHICH bead is stuck, and a bead whose session is
  gone/drained has no row at all.
- **/runs**: formula runs (1 active `mol-do-work` on claude-ci-e47j "21D" in FINALIZATION with
  "2 IN PROGRESS · 2 OPEN") + history (142). Workflow-shaped only; plain tasks never appear.
- **/mail**, **/activity** (supervisor event table, dominated by order.fired/completed),
  **/health** (supervisor/host/dolt/beads usage), **/** (ambient home: sessions, tokens, burn).
  None show per-bead status.
- There is no `/sessions` page (sessions surface inside /agents).

## 3. Findings

**F1 — the /beads fetch window hides exactly the stalled work.** The page reads the 1000
newest-created beads of a 2986-row store; ~2/3 of those rows are Slack-transcript
(`gc:extmsg-transcript`) and order-run (`order-tracking`) noise that the client then filters
away, so the window reaches back only to 2026-08-12 20:37Z. Verified against
`?status=in_progress`: the store has **9** in-progress beads; the page shows **2** (gp-6xd,
hw-6oqx2). The 7 it drops — ci-894s (in progress since 7/28 on claude-ci-e47j), ci-5xui (waiting
on Afik 6 days), hw-mma6 / hw-cn7c / hw-qd71 (in progress since Jun/May/Apr, no assignee), and two
workflow roots — are the lost subagents Taylor is asking about. The amber banner is honest but
easy to ignore. Root cause is `frontend/src/supervisor/beadReads.ts` (single created-desc window,
`BEADS_FETCH_LIMIT = 1000`).

**F2 — no stalled / waiting cue anywhere.** In-progress cards carry no assignee, age, activity,
session state or hold metadata; the attention model has no `stalled` / `waiting-on-human` reason.
The wire already carries what is needed: bead `assignee`, `metadata.gc.session_id /
gc.session_name / gc.last_heartbeat_at / gc.checkpoint_hold / gc.founder_gate / gc.checkpoint`,
and the sessions list's `state`, `last_active`, `session_name`, `alias`, `active_bead`. Only
`updated_at` is missing from the bead LIST wire (the native store list omits it; `bd show` has
it) — not needed for the cue.

**F3 — DONE is opt-in.** The done column is empty until the `closed` chip is toggled (then the
fetch re-runs with `all=true`). Acceptable; noted so nobody reads the empty DONE column as "nothing
finished".

**F4 — reachability works today.** The SPA renders over the tailnet on :8443 (hardened listener,
GET open) — see §5.

## 4. Smallest delta (proposal — not built)

All in the vendored SPA (`internal/api/dashboardspa/web/frontend`), no new routes, no new
endpoints, no Go changes; upstream-able to gastownhall/gascity-dashboard as one PR. Roughly
150–200 lines + tests, then `make dashboard-build` to refresh the committed `dist/`.

D1 (F1) **Always fetch the in-progress set whole.** In `beadReads.ts` add a second leg
`GET …/beads?status=in_progress&limit=1000` and merge (dedupe by id) into the window. The set is
tiny (9 today) and is by definition the "doing" column, so nothing in flight can fall out of the
window again. Header count and every rig's IN PROGRESS column become truthful. (Optional: same for
`?status=blocked`.)

D2 (F2) **Two new attention reasons, computed client-side from data already fetched**, in
`beadsNeedingAttention.ts` (which already drives both the NEEDS YOU panel and the nav badge):
- `waiting-human` — bead has `gc.checkpoint_hold` / `gc.founder_gate` / `gc.checkpoint =
  awaiting-human` / `hold:*` label → severity `watch`, `attention` after 2 h; summary "waiting on
  Taylor+Afik for 40h" (who = `gc.waiting_on` if stamped, else the parenthetical in the hold text,
  else "human").
- `stalled` — in_progress AND (no session resolves for the assignee via session_name / alias /
  `gc.session_id`, OR that session's state ∉ {active, awake, creating, start-pending, draining},
  OR max(session.last_active, gc.last_heartbeat_at) older than 60 min) → `attention`; summary
  "stalled 41h — no live session for claude-ci-e47j" / "session drained".
The selector gains a `sessions` input (Beads.tsx already has `sessionItems`). Because
`BeadBoardRow` already highlights rows by `attentionSeverity(beadId)`, the IN PROGRESS cards get
the existing red/amber attention treatment for free; add the reason word ("stalled 41h",
"waiting on Afik 6d") as the small label line under the title. Nav badge "Beads N" then counts
stalled + waiting.

D3 (usability, 10 lines) In-progress cards show `assignee · last activity Nm` on the label line
(assignee is on the wire; last activity = max(session.last_active, gc.last_heartbeat_at)).

Not proposed: server chips, /board route, `gc board`, done-window changes. If Afik wants the mayor
to post digests, `gc beads list --status in_progress --format=json` + the same client rule is the
non-SPA path; the parked branch already has the server-side version if that ever becomes wanted.

## 5. Reachability (Taylor / Afik)

URL: **https://tailor512g8s-mac-studio.magpie-egret.ts.net:8443/city/city/beads** (bookmark
`…/beads`; `…/agents` for the workers view). Tailnet-only — the hostname resolves and the
listener answers only for devices logged into the magpie-egret tailnet; it is NOT on the public
Funnel (the Funnel on :443 fronts other services). Reads are open, mutations require a signed
grant, so the page is effectively read-only for a browser (NEW BEAD / CLOSE will be refused).

Phone recipe (2 steps): (1) install Tailscale from the App Store / Play Store and sign in with the
same identity/org as the magpie-egret tailnet (accept the invite if one is pending); toggle the
VPN on. (2) open the URL above in Safari/Chrome — the cert is a real Let's-Encrypt cert issued via
Tailscale, so no warning. Add to Home Screen for a one-tap board. Laptop: same — Tailscale app on,
then the URL. If it does not load: check the Tailscale app shows "Connected" and that the device
is in the same tailnet (admin console → Machines).

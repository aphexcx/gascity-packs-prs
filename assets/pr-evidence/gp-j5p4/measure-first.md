# Measure first: gp-j5p4

Base: `f507f7189184df2318011cfb4a6de8a82c5dc636` (`origin/main`, merged PR 62).
Recorded before implementation/design commits. All line references below are
at that immutable base. No live adapter setting or process changed.

## Bot-specific behavior found

| Location | Base behavior |
| --- | --- |
| `slack-full/adapter/main.go:3991`; `peer_bots.go:446` | Bot events exit into a separate legacy handler before human routing, attachments, channel claims, coalescing, thread context, and delivery-receipt handling. Only plain message events and nonempty text qualify; app-mention twins and file-only posts do not. |
| `peer_bots.go:467–592` | bots.info resolution, declared/resolved identity corroboration, and self-echo guards gate admission. Registry matches bot/app/user identities; names are display-only. |
| `peer_bots.go:596–610` | **Discrepancy from the spec's base description:** unknown resolved bots also buffer. Only allowlisted peers with wake=true or immediate_channels get their own inbound; allowlisted peers otherwise buffer too. Own/unresolved bots drop. Registry entries themselves will remain unchanged. |
| `peer_bots.go:245–346` | Ambient FIFO is capped at 20/channel with oldest eviction and a dropped count. Failed immediate forwards have a separate uncapped queue only in minimal mode. Restore prepends recovery; ambient eviction can lose old context. |
| `peer_bots.go:258,348–397,630–633` | 2,000-byte per-item clip (not rune-safe), peer-bot actor prefix, read-only/do-not-reply/do-not-act header and footer; immediate full mode wraps a clipped context item instead of rendering a human reminder. |
| `peer_bots.go:640–670`; `injection_mode.go:43–45,150–209` | Minimal peer tag remains read-only; ambient peer block disappears and drains without a count. Failed-forward-only recovery clips oversized first items, restores deferred items, and emits no deferred count. Full immediate failure returns to capped ambient storage. |
| `main.go:3953–3958`; `peer_bots.go:656–670` | Event-id claim commits even on failed peer forward (queued recovery owns it). Peer path has no channel/ts claim, no deliveredIDs mark, and no full-mode context watermark update. Human path has all three; bot app-mention twins are discarded by event type. |
| `thread_context.go:309–340,419–472,723–733` | Bots alone have a 280-byte rune-safe quote cap and 1,400-byte aggregate quote allowance. Classifier excludes own/unprovable bot authors; nil classifier drops non-root bots. A bot root is exempt from filtering and has a different author line. |
| `thread_context.go:710–717,743–850` | Watermark skips non-root priors; alreadyDelivered collapse applies to admitted humans and bots alike. Newest-first bot-only allowance drops older bots while humans remain uncharged. Root bot rendering bypasses the per-post clip. |
| `main.go:4319–4366`; `reminder_budget.go:64–80,113–118,151–178`; `inbound_coalescer.go:3233–3300` | Human-only lean preamble is rendered and stored alongside full context. Budget shedding order is help, buffered peers, bot quotes, entire preamble, body tail. Bots are shed before human text. The ordinary body trim is author-neutral but conflicts with the requested whole-new-message contract. |
| `slack-full/scripts/slack_intake_common.py:560–584,1058–1062,1142–1160` | Reply-current intake excludes is_bot transcript entries and legacy peer-bot display-name prefixes. This is distinct from history read-back and remains a separate automatic anchoring policy. |
| `slack-full/scripts/slack_chat_read.py:189–260,298–303` | Channel/thread reads do **not** filter authors. Bot profile/username/ID is a display fallback. Channel read keeps newest N, but thread read stops after oldest N (the requested read-back bug). |
| `slack-full/adapter/company_routing.go:273`; `company_authors.go:226`; `company_peer.go:433–442` | Separate company-room routing resolves registered company bots, suppresses self echoes, checks membership/mentions, and correlates delegation/results. This is an authorization/routing contract, distinct from legacy peer reminder formatting. |

## Text-free counts

Fixed audit/replay window: **2026-10-14 23:00 through 2026-10-21 23:00 UTC**,
the seven days used by the progressive-disclosure note.

- Re-ran `measure_injected_alerts.py`: **575 reminders, 518,967 bytes**,
  **20,593.93 estimated tokens/day** (UTF-8 bytes / 3.6 / 7), exactly the
  published baseline. Replay preparation: 657 members, 73 batches.
- The captured reminders contain **110 delivered buffered bot items in 79
  ride-along blocks**, **0 immediate peer reminders**. 18 item copies match
  labels in the current allowlist; 92 do not. Present-day labels cannot prove
  historical identity or registry membership. These are delivered copies,
  not a complete inventory of unique Slack posts.
- Adapter service log in that same window: **150 buffered admissions**
  (49 explicitly allowlisted, 101 unknown), **0 immediate deliveries**, **0
  immediate-forward failures**, and **41 peer-context withholding events**.
  Admission does not prove eventual delivery, and the base does not log
  individual successful ride-along deliveries.
- Latest seven-day log window, **2026-10-18 20:42 through 2026-10-25 20:42 UTC**:
  229 buffered admissions (48 allowlisted, 181 unknown), 0 immediate deliveries,
  0 immediate-forward failures, 127 withholding events. Available dated service
  log ends at 2026-10-25 15:40:50; this is a coverage limit, not proof of zero
  subsequent activity.
- The journal now exists, but contains only one record beginning at
  2026-10-25 20:38:42 UTC. Its schema lacks author class and recovery membership;
  it cannot classify peer delivery paths or reconstruct the earlier seven days.

Sources were read-only: city service log, inbound journal, current registry,
and mayor transcript files. Raw extracted reminders remain in ignored
`.gc/scratch/gp-j5p4/` with private permissions. Only aggregate counts and
synthetic fixtures enter the branch. `baseline.json` and
`peer-counts-before.json` retain numeric reports. Counts distinguish observed
delivery copies from buffered admissions; neither is overstated as a complete
unique-post delivery census.

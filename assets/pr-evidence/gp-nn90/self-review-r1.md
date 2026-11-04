# gp-nn90 self-review, round 1 (the lane's own model, before READY)

- **Reviewer.** A fresh general-purpose subagent on Claude Opus 5.5, read-only. Its brief was the
  whole diff `origin/main...HEAD` in the lane worktree, against the bead spec of record, the 11/4
  deltas, the design's inbound path (line 114) and the README's contract readings. It had the code
  and the requirements, not the implementer's conclusions.
- **Head reviewed.** d4927e83.
- **Its counts.** BLOCKER 0, MAJOR 2, MINOR 9, NIT 6.
- **Its "checked and found present" list.** Every contract route; every test case in spec item 4;
  the three 11/4 cases; isolation on every city route; logging; credentials and SQL; no
  dynasty.org, account id or database id; nothing outside `whatsapp/gateway/`.
- **The fixes.** Commit 11285f36, verified by 84 tests and a 31/31 mutation check
  (`mutation-check.txt`).

## Findings and what was done

| # | Severity | Finding (reviewer's words, condensed) | Disposition |
|---|---|---|---|
| 1 | MAJOR | `src/gateway.ts` send: a timed-out or 5xx Graph call deleted its claim, so a retry under the same key sent twice. The same happened after a crash between Meta's success and the record (stale-claim takeover). This contradicts "the SAME answer again ... without a second Meta call". | **Fixed.** `sends.state` is claimed, sent or unknown. Only a 4xx refusal frees a key. No answer, a 5xx, or a 2xx without an id answers `502 outcome_unknown`, and every repeat answers `409 outcome_unknown` with no Graph call. An abandoned claim older than 60 s reads `outcome_unknown` and is never taken over. Tests cover the network failure, the 5xx and the abandoned claim. Readings posted on both beads. |
| 2 | MAJOR | Batch splitting at 400 statements could cut one invite bind across two D1 batches. A failure then lands the code as a message, with no joined row and the code still live. The chunk test never reached a split. | **Fixed.** Batches are cut only between messages (`batchRanges`). On a later batch's failure, the committed messages' welcomes still go. The new test runs 199 messages plus a stranger's code straddling a cut at 400, with the second batch failing: nothing of the bind commits. The redelivery binds once and welcomes once. The big-payload test now spans 2 batches (250 messages). |
| m1 | MINOR | A status arriving before `UPDATE sends SET wamid` commits was lost. | **Fixed.** `orphan_statuses` holds it, decided inside the transaction. The send adopts orphans when it records its wamid, and the cron drops unclaimed ones after a day. Tested with a Graph hook that delivers the status mid-send. |
| m2 | MINOR | `[observability]` invocation logs record the handshake URL, which carries the verify token. | **Fixed.** `[observability.logs] invocation_logs = false` (in wrangler 4.154's schema). The Worker's own lines stay. |
| m3 | MINOR | Bodies were read whole before the size check (unauthenticated memory pressure). | **Fixed.** `readCapped` streams and cancels at the cap: 1.9 MB for the webhook, 256 KB for city calls. |
| m4 | MINOR | Invite brute force is limited per phone only; a redelivered wrong code counted twice. | **Partly fixed.** An attempt is counted once per wamid (tested). The many-accounts residual is documented in the README "Beyond the contract" and routed to the mayor: the contract fixes 6 digits. |
| m5 | MINOR | The re-key runbook moved phones but not held deliveries, media or sends. | **Fixed** in the README runbook. |
| m6 | MINOR | Reactions not moving the window fights the window-keeper (a thumbs-up acknowledgement). | **Changed.** A reaction moves the window; system and unsupported do not. If Meta disagrees, its 131047 reads `window_closed`. Still flagged as a founders' call (README "The window"). |
| m7 | MINOR | The tampered-byte test flipped the object name, so "stores nothing" proved nothing. | **Fixed.** The byte is flipped inside the text, and the same bytes re-signed are stored (the check is what refused them). |
| m8 | MINOR | The GET /v1/window test never asserted that its three sends succeeded. | **Fixed.** It asserts `[200, 200, 200]`. |
| m9 | MINOR | The README overstated the log test (the cron and refused sends were not driven). | **Fixed.** The log test now drives `not_bound`, `window_closed`, 131047 (with a phone in Meta's error text) and `outcome_unknown`. A new test drives `src/index.ts` (webhook, refused handshake, cron) through a console spy. The README states exactly what each test drives. |
| n1 | NIT | The stale drop used the 7-day hold, but the safe bound is the 8-day dedup row; the rule was undocumented. | **Fixed.** 8 days; a 7.5-day-old first delivery lands (tested). Documented in "Beyond the contract". |
| n2 | NIT | Welcome and canned sends run before Meta's 200 (up to 20 s each). | **Reduced.** The gateway's own texts use an 8 s Graph timeout. |
| n3 | NIT | Invite expiry was checked at the receive time. | **Fixed.** It is checked at Meta's stamp of the text (tested: a text sent before the expiry and redelivered after it binds). |
| n4 | NIT | 24h01 was tested only with a reaction. | **Fixed.** A text at 24h01 is refused `window_closed`. |
| n5 | NIT | A reaction's `message_id` is not checked against the city's deliveries. | **Left.** `to` is bound to the city, so Meta bounds it to that thread. |
| n6 | NIT | The hand-written constant-time compare could use the runtime's own. | **Left.** It is correct and runtime-independent. |

Also fixed in the same round, found by the implementer: routes and methods are matched as own
properties only. A custom HTTP method named like an `Object.prototype` key reached a non-handler.

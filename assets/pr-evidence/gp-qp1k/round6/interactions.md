# Held connection and notice engine: pre-test inventory

The baseline is merge `2d86174a7852176789f70a1e70f57e08df5c9bf7`, first parent
`ad143d179fa453eed5f9ed2125193b8fcb36a739`, second parent (main)
`939ff1f3ad67ed0731a4a463095fd8f17c043a7c`. Both sides' hunks in all eleven
shared files were read before this table and before testing. The automatic
merge tree is `a10c4c56f6578f7f1aaeb3fab647ee1e419ff0d4`; the sole adjustment
moves the existing notice changelog bullet below main's held-link bullets.

| Row | Consumer / observation | Main's change under it | Test plan |
| --- | --- | --- | --- |
| A | `adapter/src/index.js` main loop calls `inbound.tick()` then `notices.poll()` each 250 ms; `never-silent.js` poll/cause reads the clock for failure, uncertain-stop and delivered-health waits | Inbox reads now use 300000 ms while held, 2000 ms while disconnected; tick remains independent | Child-process pins: uncertain stop mails immediately, owner line at 180000 ms within one tick; continuous failed receipts give both at three minutes, with no additional inbox read |
| B | `adapter/src/inbound.js` take tracks eligible new messages before journaling; `observeDelivery` feeds POST and concluded receipt results to notices | A connect/wake requests the same `poll()`/take path; no alternate receipt path | Same terminal pending receipt with zero delivered bytes in held and polling modes: enrollment, targets, observation and stop persist, one owner line and one start mail |
| C | `never-silent.js` outage grouping, durable owner reservation and start/cleared mail marks | Link up/down only changes inbox scheduling; no notice callback takes link state | Drop/reconnect while stopped: 2-second fallback reads resume, same outage id, one line/start mail and no cleared mail |
| D | `inbound.js` track uses `created: clock()`; `never-silent.js` result/stop default to `clock()`; oldest age uses created | Lost wake on a healthy link can postpone admission until the 300-second safety read | No enrollment or notice before admission; provider timestamp stays old, created/observation/stop are adapter arrival time; the three-minute line wait starts then |
| E | `commands/status.sh`, `commands/status/help.md`, listener status in `adapter/src/index.js` and `tests/test_pack.py` | Main adds inbound link state and its plain output below enrollment; PR adds notice status and JSON coordinate filtering | Pack matrix: held/poll and notices on/off, both sections in plain and JSON output |
| F | `gateway/src/gateway.ts` send completion and accepted-attempt headers | Main wakes adopted statuses after the durable send batch; PR returns fresh/replayed headers and original accepted attempt | Gateway test: early status, local held socket, fresh send with attempt, repeat with different attempt; one provider send, one wake, original attempt on replay |
| Unchanged clocks | `never-silent.js`: health freshness; activity timestamps; owner cooldown; seven-day count; mail retry/lifetime; outage start/end | Nothing: main does not change this file or these clock consumers | Existing notice class suite plus A-D |
| Unchanged inbox consumers | `inbound.js`: startup journal replay, pending receipt readback, markDelivered, deadLetter and operator resolution | No changes to receipt/stop/forget callbacks; keeper bind retry moved out of the inbox poll onto its own ordinary cadence | Existing adapter suite and unchanged link-engine tests, plus B/C |

Each shared file retains both behaviors:

- `whatsapp/adapter/src/inbound.js`: wake/poll scheduling feeds the same admission and receipt callbacks; notices never consume link state.
- `whatsapp/adapter/src/index.js`: held-link config and notice construction coexist, with notice polling still in the main loop.
- `whatsapp/adapter/test/fakes.js`: local wake sockets and the default-off link fixture coexist with registration failure and receipt-byte controls.
- `whatsapp/CHANGELOG.md`: main's held-link entries precede the existing notice entry under the same Unreleased heading; this is the merge's only manual adjustment.
- `whatsapp/commands/status.sh`: the link line remains directly below enrollment, alongside the notice section; JSON retains both objects.
- `whatsapp/commands/status/help.md`: both link fields and notice fields are documented, including JSON output.
- `whatsapp/gateway/README.md`: held-link deployment and replay-attribution migration documentation coexist; migration 0005 remains required for the notice PR.
- `whatsapp/gateway/src/gateway.ts`: the adopted-status wake follows the durable batch, then sendAnswer returns fresh/accepted-attempt headers; repeatAnswer returns the stored attempt without another send or adoption wake.
- `whatsapp/gateway/test/gateway.test.ts`: main's wake-failure privacy assertions coexist with replay and accepted-attempt assertions.
- `whatsapp/README.md`: held-link options coexist with the opt-in notice contract; no timings or owner lines change.
- `whatsapp/tests/test_pack.py`: main's plain link assertions and the PR's JSON privacy assertions coexist; E adds their combined matrix.

No production behavior change is planned. A-D are pins at the merge commit.
For B, E and F, one-sided-hunk checks will demonstrate that the tests detect
losing either behavior where a side's hunk can be removed independently.
All fixtures and endpoints are local/invented; no live state is read.

# gp-74i1: ingestion alarm baseline

Measured 2026-11-09 from the assigned lane at base
`2372b4f262aad8e847c416267ba3092d31a083fd`, before changing adapter behavior.

## Sources and method

- Read all seven retained inbound journals, `20261103.jsonl` through
  `20261109.jsonl`, under `/Users/tailor512/city/.gc/slack/inbound-journal/`.
  Select `status=delivered_unseen, alerted=true`: 18 receipt rows.
- Correlate the service log at
  `/Users/tailor512/city/.gc/services/slack/logs/service.log` with each receipt's
  channel, message/root timestamp, target session, hand-off and alert time.
  Its timestamps are America/Chicago; journal timestamps are UTC. On these
  dates the offset is six hours. `measure.py` performs the conversion.
- Read six distinct threads using bounded `gc slack read --json` commands;
  `thread-reads.manifest` records each command and exit status. Every read
  returned `has_more=false`. The CLI omits reaction and bot-id fields, so
  `read_facts.py` uses the existing read helper for read-only Slack API calls:
  `auth.test`, bounded `conversations.replies`, and 18 `reactions.get` reads.
  `slack-facts.json` retains only IDs, timestamps, attribution and reactions.
- The bot is `B0BGYLTM8NT` / `U0BHE265YNN`. Matching publish calls identify
  session `ci-wisp-k95`. The three source papercuts were read in full.

## Alarm baseline and qualification

**14 of 18 alerts have evidence of the bot's hand before the alert**:
six have a published reply corroborated by Slack's immutable message
timestamp; eight additional receipts have a pre-alert `/react` call and the
bot's reaction is still present. No `reactions.add` error appears in the
service log during this period. The latter eight are reconstructed historical
reaction evidence: Slack exposes no reaction creation timestamp, and the old
handler logs the call before success. They must not be described as durable
success receipts.

The remaining four have no qualifying hand in the measured interval. These
are **four eligible alarms under the proposed rule**, not proof that a model
never received the input. Thus the defensible split is six directly verified
false alarms, eight strongly corroborated false alarms, four without prior
hand evidence. A binary reconstructed baseline is 14 false / 4 eligible.

| Receipt | Message timestamp | Pre-alert evidence |
| --- | --- | --- |
| ir-87615-50 | 1793948327.314169 | none |
| ir-34177-7 | 1794045587.926179 | none; first logged eyes is after alarm |
| ir-34177-10 | 1794047550.590319 | reaction call, bot eyes still present |
| ir-34177-14 | 1794063933.743159 | reply 1794064037.349039 |
| ir-34177-39 | 1794159381.215279 | reply 1794159511.465839 and reaction call |
| ir-34177-40 | 1794159412.634159 | same reply and reaction call |
| ir-34177-41 | 1794159424.547469 | same reply and reaction call |
| ir-34177-42 | 1794161439.068439 | reply 1794161521.556929 |
| ir-34177-53 | 1794168260.845629 | none in retained call log/thread interval |
| ir-34177-57 | 1794181291.507919 | reaction call, bot eyes still present |
| ir-34177-61 | 1794181804.000289 | reply 1794181894.092999 and reaction calls |
| ir-34177-71 | 1794182138.935059 | none; eyes 6.598 seconds after alarm |
| ir-34177-72 | 1794182172.506869 | reaction call, bot eyes still present |
| ir-34177-73 | 1794182191.156699 | reaction call, bot eyes still present |
| ir-34177-74 | 1794182219.663679 | reaction call, bot eyes still present |
| ir-34177-75 | 1794182250.463389 | reaction call, bot eyes still present |
| ir-34177-76 | 1794182277.390449 | reaction call, bot eyes still present |
| ir-34177-90 | 1794188916.796289 | reaction call, bot eyes still present |

The literal claim that every burst line had eyes before its alarm is false:
`ir-34177-71` was handed off at 23:55:51.240517Z, alerted at
23:58:54.402231Z, and its eyes call was at 23:59:01Z on November 8.
There is no bot reply within that interval. This correction does not negate
the observed false-alarm mechanism, which has six directly timestamped reply
examples and eight corroborated reaction examples. The burst's six alerted
lines in C0CCMJ1DU02 are not eight alerts in that channel; the two other alerts
in the cited interval belong to C0BKF28CYUE and C0AMHQ74ZLK.

## Late-proof timing

Seven `late=true, status=ingested` rows were recorded on November 8–9.
Lag is the **proof timestamp minus DeliveredAt**, not the polling time.
Sorted seconds: 450.061, 710.301, 731.389, 752.670, 776.860, 788.974,
812.321. Nearest-rank p95 is **812.321 seconds (13m32.321s)**; rounding up to
whole seconds gives 813 seconds. The observer detected the last sample after
817.078 seconds, reflecting polling overhead. The proposed ten minutes does
not cover six of these seven samples.

This is a small, conditional sample of late receipts, six from one turn; it
does not estimate the p95 of all handoffs. Any window decision must preserve
this limitation and the existing two-hour late-proof horizon.

## Current observer and coverage

`ingestion_observer.go` defines a three-minute initial window and a two-hour
late-proof horizon. `step()` only checks rendered transcript evidence, then
mails once per receipt. The handler paths already distinguish explicit
`/react` calls from company admission acknowledgements; publish success is
centralized in `recordDeliveredPublish`. The reaction request currently has
no caller session attribution, whereas publish does.

`ingestion_observer_test.go` provides `ingestionTestRecord`,
`ingestionTestEntry`, a temporary journal and injected `now`, `fetch` and
`notify`. Existing step cases cover busy-session quoted inputs, pre-deadline
suppression, rendered fanout, durable notification retry, restart dedup and
one receipt shared across targets. Companion ingestion test files cover late
proof, retention, canonical-session resolution, follow-up timing, interrupted
journal writes and opt-in live delivery. These fixtures can support the new
reaction/reply and channel-batch cases without a live Slack test.

The baseline test command and results are recorded separately in
`baseline-go.manifest` and `baseline-go.jsonl`.

Baseline validation: `go test -json -run '^(TestIngestion|TestInboundJournal)' ./...`
from `slack-full/adapter`, Go `go1.27.3 darwin/arm64`, exit 0. There were
31 passing top-level tests (52 including subtests), and the opt-in
`TestIngestionLiveBusySessionProof` was skipped. No failure. These are tests
of the unchanged observer, not validation of a proposed fix.

## Measurement handoff

Mail `ci-wisp-g0gjb8m` sent the corrected baseline and timing to `mayor` under
the bead's explicit step-2 stop-and-mail clause. Ruling `ci-wisp-gozwdyu`
accepted the corrected baseline and authorized steps 3–6. The initial window
stays three minutes: seven conditional samples do not establish routine delay.
The lane delivers a draft PR and READY; the mayor takes ownership and keeps
the bead open for rollout and the week of observation. No live pin change or
post-roll observations are part of this implementation delivery.

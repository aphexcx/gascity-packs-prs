# gp-41p6: delivered versus ingested

Base: ec42f23490a123903fd284eec6d08752d6b29a92. Local record:
`/Users/tailor512/city/assets/ops/records/slack-ingestion-20261030/gp-41p6/`.
The full claimed spec, selected original journal rows, structured transcript
evidence, validation logs and completion manifest are retained there.

## Measurement before changing code

| Slack message | gc receipt | tmux receipt | bytes | harness enqueue/render |
| --- | --- | --- | --- | --- |
| 1793396994.219339 | ir-93767-158 | nr-93767-177 | 2373 | absent |
| 1793400117.363949 | ir-93767-172 | nr-93767-191 | 1963 | absent |
| 1793400187.460319 | ir-93767-173 | nr-93767-192 | 693 | both present |
| 1793400217.435699 | ir-93767-174 | nr-93767-193 | 1210 | absent |
| 1793400533.063889 | ir-93767-175 | nr-93767-194 | 3526 | both present |
| 1793400564.044799 | ir-93767-176 | nr-93767-195 | 628 | both present |
| 1793400917.910839 | ir-93767-177 | nr-93767-196 | 1841 | absent |

All seven transport receipts report complete payloads and `submitted=true`.
The adapter used `POST /v0/city/{city}/extmsg/inbound` with
`X-GC-Delivery-Receipt: require`. The raw inbound journal is the relevant
receipt surface for these events, not the separate company-room ingress store.

The two mayor transcripts were
`87275b55-c774-4c9d-9013-898e1c0f7b3c.jsonl` and
`1a4163cb-9eb3-4b65-a431-0f8e2f6ca888.jsonl`, under the city Claude project.
The three surviving lines have enqueue, `absorbed_mid_turn` removal, rendered
`queued_command` attachments, and user API text. The four missing lines appear only
in later diagnostic/manual tool reads. Those are excluded from ingestion proof.
The current nudge state had no mayor pending items; it is not a historical
snapshot and cannot prove the past queue contents. `gc session peek` likewise
shows the present pane, not the missing historical input.

The fourth loss, reported by the mayor at 00:34Z, is included in
`incident-journal.jsonl`. Its 22:42:07.635513Z adapter row reports 1,679 payload
bytes and one quoted-context line. The transport reports 1,963 framed bytes.
The only transcript hits are a 22:50:28 manual tool result and its API snapshot;
neither is ingestion. The local record retains `journal-fourth.jsonl`,
`transport-fourth.log`, `transcript-fourth.json` and the extraction script.

Supervisor evidence is in `~/.gc/supervisor.log`, and adapter evidence in
`.gc/services/slack/logs/service.log`. The loss is between a tmux transport write
and harness enqueue. No historical evidence distinguishes an input-loop loss
from an unsubmitted draft. gc's `submitEnterAndConfirm` accepts any busy sample
after Enter, without establishing a new transition; it can therefore falsely
confirm a new paste while the target was already busy. Fork bead `gf-b78` owns
that change and was routed by the mayor. GitHub issue creation was attempted,
but aphexcx/gascity has issues disabled (papercut pc_e351afea85e3).

## Adapter change

The journal stores a complete payload SHA-256 and proof format, never message
text. The observer accepts actual rendered Claude/Codex user records, matches
the complete payload and extmsg coordinates, and writes `ingested` with entry
evidence. After two minutes without proof it writes `delivered_unseen`, sends
a deduplicated mayor mail, and records completion. Disk journal reconstruction
preserves restart recovery. The test reproduces three lines inside thirty
seconds with quoted-context windows, the missing middle input, a later manual
read that must not count, and exactly one alert across observer restart.

No message is automatically replayed. No peer-city delivery, company receipt
state machine, dead-letter semantics, production binary, or founder-gap-watch
manifest is changed. Bounds and unsupported-format behavior are documented in
`slack-full/docs/inbound-ingestion.md`.

## Validation

Go 1.27.3 darwin/arm64; Python 3.13 in an isolated uv pytest environment.
No Node or pnpm commands were used.

- Before implementation: `go test -run '^TestInboundJournalCarriesSessionProofIdentity$' -count=1 .`
  in `slack-full/adapter` failed because the delivered row lacked proof identity.
- `go test -race -json -count=1 ./...` in `slack-full/adapter`: exit 0,
  1,410 top-level tests / 12,792 tests including subtests passed; one skipped.
- Same command in `slack-full/cli`: exit 0, 199 top-level tests / 237 including
  subtests passed; none skipped.
- `uv run --no-project --python 3.13 --with pytest --with PyYAML --with jsonschema python -m pytest tests -q`
  in `slack-full`: exit 0, 907 passed, two skipped. Two existing fork-in-thread
  deprecation warnings in `test_slack_company_outbound.py`.
- After Codex proof support and the opt-in live harness:
  `go test -race -json -run '^Test(Ingestion|InboundJournal)' -count=1 .`
  in `slack-full/adapter`: exit 0, ten passed, one intentionally skipped live test.

The live proof and review results will be recorded before handoff. The mayor
alone retires the founder gap watch after a week with zero unseen rows.

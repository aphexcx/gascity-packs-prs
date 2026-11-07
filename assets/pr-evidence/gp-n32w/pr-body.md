Human messages can wait through coalescing before an agent shows it has seen
them. This adds an optional arrival-time acknowledgement: `ACK_REACTION` picks
a fixed emoji, `auto` keywords, or a small `model`, while an unset value keeps
the existing behavior. The busy hourglass still tracks targeted work and is
removed on reply; the acknowledgement remains.

This ports Taylor's Hou installation patches from 2026-10-30 and 2026-11-03,
on Afik's request to open a PR for consideration. The read-only source is
a citadel record (the mayor's).
The base is fork `main` at `2372b4f262aad8e847c416267ba3092d31a083fd`, which
includes #68 and #72. No installation is repointed by this PR.

Round 1 imported the two implementation files with only attribution changes;
round 2 shares and bounds the acknowledgement path across ingress routes.
The imported `ack_model_test.go` remains byte-identical.
The main hook additionally treats Slack `already_reacted` as benign, reports
other Slack API errors, avoids per-message missing-key warnings/channel
lookups, and rejects timeout duration overflow. The README explains the five
settings, endpoint/data flow, fallback and reaction lifetimes; the doctor
checks optional key-file readability without sourcing or exposing contents.
There was no busy section or optional-file doctor pattern in the current
tree, so those are added. A four-line indentation-only cleanup in
`company_thread_ambient.go` fixes a pre-existing base formatting issue so
the required module-wide `gofmt -l .` is empty.

### Design note (Afik's adoption decision)

| Path | When it lands | Emoji selection | Per-message cost | Needs |
| --- | --- | --- | --- | --- |
| A. Existing adapter busy reaction | Targeted dispatch; removed on reply | Configured `BUSY_REACTION`, default `hourglass` | Slack add/remove; no model | Token, targeted dispatch, pending-mark registry |
| B. Agent-side reaction | Once the main agent turn reaches the reaction tool | Agent follows its reaction skill | Main-model inference plus reaction tool call; can share an existing turn | Agent instruction and `gc slack react` or gchat equivalent |
| C. This adapter acknowledgement | Legacy arrival before routing/coalescing; company first delivery attempt; stays as a receipt | Fixed emoji, keyword `auto`, or small `model` | One Slack add attempt; keywords use no inference; model uses one request, max 16 completion tokens, default 6 s timeout | `ACK_REACTION`; model also needs endpoint, model name and readable key file |

PR #79's gchat skill says “Acknowledge with a reaction, not a message” and
asks the agent to choose an emoji fitting the feeling or topic. That tree
does not establish a separate Bonanza-side reaction model. This PR uses
OpenAI-compatible chat completions with keyword fallback on every model
failure. Hou uses the Teale gateway; a local OpenAI-compatible server can be
selected with `ACK_MODEL_BASE_URL` without a code change, provided it serves
that API and the operator supplies its model/key configuration.
The timeout bounds the model request; channel lookup and Slack delivery are
additional. Both marks are asynchronous and may arrive in either order.
Choose distinct acknowledgement and busy emojis: identical names share one
Slack reaction and cannot have separate lifetimes. Round 2 rejects a fixed
acknowledgement equal to the busy emoji at configuration time.

I recommend keeping the shipped default off and trying `auto` on Hou first,
with the busy hourglass retained for progress. Enable a local `model` only
after comparing its emoji quality and latency with the keyword baseline.

Afik: adopt (C) on every city with which mode, or keep it Hou-only?

### Validation (round 1)

- Go 1.27.3: `go test ./... -count=1 -json` from `slack-full/adapter`:
  1,458 top-level tests passed, 3 skipped (12,891 passing cases including
  subtests), both packages passed; `go vet ./...` passed.
- Focused acknowledgement suite: 28 passing cases; the same selection
  passed under `go test -race ./... -run 'Test(Ack|ParseAck|PickAck)' -count=1`.
- Model tests use local `httptest` servers and a loopback failure sentinel.
  Runs point the model default and HTTP/HTTPS proxies at a closed local
  endpoint, bypassing proxies only for loopback fixtures.
- Python 3.12 shared venv: `python -m pytest tests -q` from `slack-full`:
  914 passed, 2 skipped; two existing `os.fork()` deprecation warnings.
- Targeted/ambient/bot/redelivery behavior, no-key fallback, disabled mode,
  config validation and doctor file handling are covered. Existing pack
  tests and `busy_reaction.go` are unchanged.

Evidence and source anchors: `assets/pr-evidence/gp-n32w/README.md`.
Draft only; mayor's Codex gate and Fable read remain pending.

### Round 2

The review delta starts at `bb0515de59a1e912c32a12e3bfe8afdecadcedf7`.
The company gateway uses the same persistent receipt when configured, with
its existing posting token and admission timing. The unset lifecycle is
unchanged. Paired Slack events share one pick and post, and four shared slots
queue overflow without dropping acknowledgements.

| Finding | Commit | Regression evidence |
| --- | --- | --- |
| Gate MAJOR / M1: company rooms and registered agent DMs | `e4c794c6` | Signed HTTP admissions each post one auto pick with the path's token; existing unset lifecycle tests pass. |
| M2: paired message/app_mention events | `11875d26` | One in-flight model pick and post per channel/timestamp; distinct timestamps remain independent; bounded/expiring registry test. |
| M3: doctor's env precedence | `c878abe9` | Env-file assignment wins over inherited values, including empty; absent assignment preserves inheritance. |
| M4: concurrent acknowledgement work | `15b838ff` | Nine messages peak at four model requests and all nine receive their receipt. |
| M5: fixed ack/busy collision | `4f153813` | Configuration rejects equal names after colon normalization and names both settings. |
| N2 / N5: public text and key-path disclosure | `1e801323` | Public-body audit passes; README distinguishes key paths from key contents. |

Go 1.27.3 validation: `go test ./... -count=1 -json` passed both adapter
packages, with **1,463 top-level passes, 3 skips**, and **12,898 passing cases
including subtests**. `go vet ./...` passed and `gofmt -l .` was empty.
The focused race run passed **35 top-level tests / 45 cases**. Python 3.12:
`python -m pytest tests -q` passed **915 tests, 2 skipped**, with two existing
fork deprecation warnings. All tests use local fixtures.

The four Go behavior regressions also fail against the original round-1
production source via a Go overlay; the doctor and document checks were red
before their fixes. The full validation manifest and per-item logs are in
the mayor's round-2 evidence record. The worker self-review is complete;
the mayor's round-2 delta gate and Opus 5.5 read are pending. PR remains draft.

N1, N3, N4 and N6 were read and accepted: model data exposure is documented;
the round-1 focused count included 13 existing company tests; the four
round-1 hardening changes are retained; and true event-ID redelivery is
filtered before the legacy hook. N5 is now explicit in the README: a fallback
log can contain the key-file path, never the key contents.

### Round 3

Both round-2 reviews found that the ack limit could hold dispatch and company
delivery. At `5fce97fac5a555c12507145909ddd090716560e5`, nine signed HTTP
admissions at `SLACK_DISPATCH_CONCURRENCY=1` returned 200, but a held model
allowed only four messages to forward; after release only five had forwarded,
with messages six through nine logged as dropped. The regression now passes:
all nine forward while the model is held, all nine eventually receive one ack,
and model concurrency peaks at four.

| Finding | Commit | Regression evidence |
| --- | --- | --- |
| Gate/read MAJOR: ack backpressure delays or drops messages; read MINOR: filtered messages take slots | `41b48bb6` | HTTP admission test above; company room/DM delivery finishes with a held model; bot and system messages finish with every ack slot occupied. |
| Gate/read MINOR: company model requests lack channel names | `1c1eeda7` | Real adapter startup and signed HTTP company-room admission: baseline prompt lacks a channel; fixed prompt includes `Channel: #company-launch`. |
| Read NOTE: MPIM coverage and README wrapping | `15a81266` | Signed HTTP MPIM admission delivers and posts exactly one configured ack with the owner's token; key-path prose rewrapped. |

The semaphore is acquired inside the ack goroutine after the bot/subtype
filter and identity claim. Dispatch and company delivery do not wait for
ack capacity or model/Slack requests. Pending acks remain in memory without
a queue cap. The channel-name cache is initialized before the gateway copies
cfg, sharing the same cache with legacy dispatch.

In configured mode, company receipts ignore `SLACK_COMPANY_VISIBLE_ACKS` and
replace the eyes/check/warning terminal lifecycle with one persistent ack.

Go 1.27.3: `go test ./... -count=1 -json` from `slack-full/adapter` passed
both packages: **1,466 top-level passes, 3 skips; 12,907 passing cases**.
`go vet ./...` passed; `gofmt -l .` was empty.
`go test -race . -run '^TestAck' -count=1 -json` passed **32 top-level tests,
40 cases**. Python 3.12.13: `python -m pytest tests -q` from `slack-full`
passed **915 tests, 2 skipped**, with two existing fork deprecation warnings.
Both requested regressions were red on the exact round-2 head. Tests use
local fake Slack, gc, and model endpoints. Worker self-review is complete;
the Mayor's round-3 delta gate and Opus read remain pending. PR stays draft.

Round-3 logs and the completion manifest are in the city evidence directory
`assets/pr-evidence/gp-n32w/round-3/`.

Beads: gp-n32w (round 1), gp-82fs (round 2), gp-1nez (round 3)

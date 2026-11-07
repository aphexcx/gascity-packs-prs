Human messages can wait through coalescing before an agent shows it has seen
them. This adds an optional arrival-time acknowledgement: `ACK_REACTION` picks
a fixed emoji, `auto` keywords, or a small `model`, while an unset value keeps
the existing behavior. The busy hourglass still tracks targeted work and is
removed on reply; the acknowledgement remains.

This ports Taylor's Hou installation patches from 2026-10-30 and 2026-11-03,
on Afik's request to open a PR for consideration. The read-only source record
is a **citadel path**:
`/Users/tailor512/city/assets/ops/records/hou-ack-reaction-patch-20261107/pull.out`.
The base is fork `main` at `2372b4f262aad8e847c416267ba3092d31a083fd`, which
includes #68 and #72. No installation is repointed by this PR.

The two imported implementation files differ from that record only in their
attribution headers. The imported `ack_model_test.go` is byte-identical.
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
| C. This adapter acknowledgement | Starts on human arrival, before routing/coalescing; stays as a receipt | Fixed emoji, keyword `auto`, or small `model` | One Slack add attempt; keywords use no inference; model uses one request, max 16 completion tokens, default 6 s timeout | `ACK_REACTION`; model also needs endpoint, model name and readable key file |

PR #79's gchat skill says “Acknowledge with a reaction, not a message” and
asks the agent to choose an emoji fitting the feeling or topic. That tree
does not establish a separate Bonanza-side reaction model. This PR uses
OpenAI-compatible chat completions with keyword fallback on every model
failure. Hou uses the Teale gateway; the city's recorded local endpoint at
`127.0.0.1:11438` would use
`ACK_MODEL_BASE_URL=http://127.0.0.1:11438/v1` without a code change, provided
it serves that API and the operator supplies its model/key configuration.
The timeout bounds the model request; channel lookup and Slack delivery are
additional. Both marks are asynchronous and may arrive in either order.
Choose distinct acknowledgement and busy emojis: identical names share one
Slack reaction and cannot have separate lifetimes.

I recommend keeping the shipped default off and trying `auto` on Hou first,
with the busy hourglass retained for progress. Enable a local `model` only
after comparing its emoji quality and latency with the keyword baseline.

Afik: adopt (C) on every city with which mode, or keep it Hou-only?

### Validation

- Go 1.27.3: `go test ./... -count=1 -json` from `slack-full/adapter`:
  1,458 top-level tests passed, 3 skipped (12,891 passing cases including
  subtests), both packages passed; `go vet ./...` passed.
- Focused acknowledgement suite: 28 passing cases; the same selection
  passed under `go test -race ./... -run 'Test(Ack|ParseAck|PickAck)' -count=1`.
- Model tests use local `httptest` servers and a loopback failure sentinel.
  Runs set `ACK_MODEL_BASE_URL=http://127.0.0.1:1/v1` and HTTP/HTTPS proxies
  to that closed loopback port, bypassing proxies only for loopback fixtures.
- Python 3.12 shared venv: `python -m pytest tests -q` from `slack-full`:
  914 passed, 2 skipped; two existing `os.fork()` deprecation warnings.
- Targeted/ambient/bot/redelivery behavior, no-key fallback, disabled mode,
  config validation and doctor file handling are covered. Existing pack
  tests and `busy_reaction.go` are unchanged.

Evidence and source anchors: `assets/pr-evidence/gp-n32w/README.md`.
Draft only; mayor's Codex gate and Fable read remain pending.

Bead: gp-n32w

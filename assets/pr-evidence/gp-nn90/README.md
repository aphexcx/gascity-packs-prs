# gp-nn90 evidence: the WhatsApp gateway Worker (`whatsapp/gateway/`)

- Base: aphexcx/gascity-packs-prs main 8cb01a83bdaa6b4b42aeb63bae1ee4a6d369262f.
- Toolchain: `/Users/tailor512/city/.gc/shims/toolchain/pnpm` (pnpm 11.20.0), Node v24.21.0
  (`pnpm exec node --version`).
- Every transcript names its own head in its header.

| File | What |
|---|---|
| `vitest-green.txt` | `pnpm exec vitest run --reporter=verbose` on the committed implementation: every case by name and the counts (workerd + a real D1 + a fake Graph API). |
| `vitest-red.txt` | The same suite against API-only stubs of `src/gateway.ts`, `src/meta.ts` and `src/window.ts` (exports present, bodies "not implemented" or 501): every case fails. The committed files are restored afterwards and re-verified by blob hash against HEAD. |
| `typecheck.txt` | `pnpm typecheck` (`tsc --noEmit`). |
| `wrangler-dry-run.txt` | `pnpm exec wrangler deploy --dry-run --outdir dist`: the bundle builds with no login, and the bindings it shows name no account and no database id. |
| `mutation-check.txt` | One mutation per rule the contract or the bead names, each run against the suite (`/tmp/gp-nn90-work/mutate.py`): the suite must go red on every one. |
| `self-review-r1.md` | The lane's self-review (a fresh reviewer on the lane's own model) with each finding's disposition. |
| `codex-r1-blocked.txt` | The worker's codex DEEP round as attempted at 19:05Z 11/4, refused by the codex account's usage limit ("try again at 6:31 PM", 00:31Z 11/5). No review ran. |
| `codex-deep-prompt.md` | The DEEP prompt for the codex round (the key-holder focus list). |
| `bead-comment-contract-readings-1.md`, `-2.md` | The contract readings posted on gp-nn90 and gp-9hf1: the first before the code, the second after the self-review. |

## The 11/4 behaviours, by test name (`vitest-green.txt`)

- **HOLD:** "a delivery is held, body and all, and returned by every poll until the ack names its id".
- **ACK OF A DELETED ID:** "an ack for an id the cron already deleted is a no-op 200".
- **REPEATED ACK:** "acking an id an earlier ack already deleted is the same no-op, and touches
  nothing else".

## Logs

Two tests check every log line against the scenario's phone numbers, message text, profile name,
invite code, credential and the four secrets:

- "no log line carries a phone number, a message body, a token or a secret" (the gateway's paths);
- "the Worker entry's own console lines ..." (`src/index.ts`).

`grep -n console whatsapp/gateway/src/*.ts` shows the only two `console.log` calls, both in
`src/index.ts`. Each logs a JSON object of an event name and counts.

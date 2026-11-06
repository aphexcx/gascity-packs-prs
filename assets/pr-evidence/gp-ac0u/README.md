# gp-ac0u evidence: business-scoped user ids in the WhatsApp gateway (`whatsapp/gateway/`)

- **Base.** PR #73's head 392f9c09649dd37eea414dbc520270ba586ed18c, branch `feat/gp-nn90-whatsapp-gateway`.
- **Commits.** a2d65a16 is the change. e71714ab is the self-review's round 1. f29bc657 is a
  retention test the mutation check showed was needed. The evidence commit follows.
- **Toolchain.** `/Users/tailor512/city/.gc/shims/toolchain/pnpm` (pnpm 11.20.0, Node 24).
- **Headers.** Every transcript names its own HEAD in its header.
- **Regenerating.** Two scripts, run from the worktree root:
  - `python3 -I assets/pr-evidence/gp-ac0u/evidence.py "$PWD" <pnpm> <out dir>`
  - `python3 -I assets/pr-evidence/gp-ac0u/mutate.py "$PWD/whatsapp/gateway" <pnpm>`

  Each one writes the committed files back and checks them by sha256 against HEAD.

| File | What |
|---|---|
| `vitest-red.txt` | **The first RED (22 failed, 86 passed).** The new tests as first written, run against 392f9c09's `src/` and migrations, which were unchanged (the header shows an empty `git diff --stat`). The 2 that passed are guards that hold on the base by design: "a message naming no id is skipped". |
| `vitest-red-final.txt` | **The final RED (33 failed, 84 passed).** The committed tests run against 392f9c09's `src/` with `0002_bsuid.sql` absent. The 33 are 31 new cases and 2 existing tests re-cut for the schema: their direct `INSERT INTO phones` now names `conversation_id`, and their exact window objects carry `user_id: null`. |
| `vitest-green.txt` | **GREEN: 117/117** (84 existing cases, 33 new), each case by name. |
| `typecheck.txt` | `pnpm typecheck` (`tsc --noEmit`). |
| `wrangler-dry-run.txt` | `wrangler deploy --dry-run`: the bundle builds with no login, and the bindings name no account id and no database id. |
| `deploy-gap.txt` | **The deploy gap.** 392f9c09's `src/` (the Worker still serving) on migrations 0001 and 0002, the gap between README Deploy steps 3 and 4. A bound owner's message, a send, its status and the window work. A stranger's text and an invite text answer 500. The test is `deploy-gap.check.ts`. |
| `mutation-check.txt` | **The mutation check.** gp-nn90's 31 mutations, re-cut where gp-ac0u changed the line, plus 43 that each break one gp-ac0u rule. Every one must turn the suite red. Two equivalent mutants are listed apart, each with its reason, and are not counted. Each removes a rule that another statement of the same D1 batch already enforces. |
| `self-review-r1.md` | The self-review's findings and their dispositions. |

## The bead's ACCEPTANCE, by test name (`vitest-green.txt`)

- **A user_id with no wa_id routes, is welcomed and is delivered:** "an invite text carrying only a
  user_id binds the owner by it, welcomes it at the BSUID in `recipient`, and its messages reach
  only that city".
- **A send to a BSUID reaches Meta unchanged:** "a send to a BSUID reaches Meta in `recipient`, byte
  for byte and with no `to`; a send to the wa_id goes in `to`", and "a malformed BSUID is 400 ...".
- **The window moves on either id:** "the window moves on a message from either id ...".
- **The existing wa_id-only fixtures pass unchanged:** every case in `test/gateway.test.ts`. The
  fixtures `text.json`, `reaction.json`, `image.json`, `audio.json`, `status-sent.json` and
  `status-failed.json` are untouched.
- **The admin listing never prints a full id:** "the admin listing shows a phone's last four digits
  and 'id' for an owner with no phone, never a full id".
- **No user_id or username in a log line:** "no log line carries a user_id, a username or a phone on
  the BSUID paths".

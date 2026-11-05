# gp-0g2s evidence: WhatsApp gateway round 2 (`whatsapp/gateway/`)

- **What it fixes.** Two gate records:
  - PR 76's codex gate r1: `/Users/tailor512/city/assets/ops/codex-gates/gp-ac0u-pr76-r1-1e0a2483e.txt`, two MAJORs.
  - PR 73's record gate: `/Users/tailor512/city/assets/ops/codex-gates/gp-nn90-pr73-r1-392f9c096.txt`, a MAJOR and a MINOR.
- **Base.** PR 76's head 1e0a2483ebaf7bfa8783d28cbc128fd37db10e5a, branch `feat/gp-ac0u-whatsapp-bsuid`, on main 54f53be7.
- **Commits.**
  - 735e5373 is the change.
  - 48e53c7d is the self-review's round 1, which restructured routing (`self-review-r1.md`).
  - 8dab7afb answers the mutation check on 48e53c7d: 4 survivors (`mutation-check-48e53c7d.txt`).
  - 43f64947 answers the check on 8dab7afb: 1 survivor, a real case (`mutation-check-8dab7afb.txt`).
  - f00647e2 is README only, and the evidence commit follows. No commit after 43f64947 changes a file under `src/`, `test/` or `migrations/`, so every transcript run at 43f64947 stands for the head.
- **Toolchain.** `/Users/tailor512/city/.gc/shims/toolchain/pnpm` (pnpm 11.20.0, Node 24).
- **Headers.** Every transcript names its HEAD, or says which tree it ran on.
- **Regenerating.** Run from the worktree root:
  - `python3 -I assets/pr-evidence/gp-0g2s/evidence.py "$PWD" <pnpm> <out dir>`
  - `python3 -I assets/pr-evidence/gp-0g2s/mutate.py "$PWD/whatsapp/gateway" <pnpm>`. Pass `--check` to only count the patterns.

  Each script writes the committed files back and checks them by sha256 against HEAD.

| File | What |
|---|---|
| `vitest-red-first.txt` | **The first RED: 10 failed, 120 passed.** The round's tests, as first written, ran against 1e0a2483's `src/` and migrations, unchanged. |
| `vitest-red-review-r1.txt` | **The self-review round's RED: 10 failed, 129 passed.** 48e53c7d's tests ran on 735e5373's tree (src and migrations). |
| `alias-first-red.txt` | The one test the lane wrote after its code: routeKey put back to current-first fails exactly that test. The header says why. |
| `vitest-red-mutation-round.txt` | **The mutation round's RED: 1 failed, 138 passed.** 8dab7afb's tests ran on 48e53c7d's code: two previous spans holding one stamp went to A. |
| `vitest-red-mutation-round-2.txt` | **The second mutation round's RED: 1 failed, 138 passed**, on 8dab7afb's code. A's BSUID-shaped retry, while the records disagreed, took the number from B. |
| `vitest-red-final.txt` | **The final RED: 23 failed, 117 passed.** The committed tests ran against 1e0a2483's `src/`, with 0003 absent. |
| `vitest-red-schema.txt` | **20 failed, 120 passed.** The same run with 0003 present, so no failure is a missing table. 19 of the 20 are behaviours the round added. The 20th, "a split payload whose second batch fails", is calibrated to this round's 4 statements per message. On the base's 2, the whole payload fits in one batch, so the second never fails. |
| `vitest-green.txt` | **GREEN: 140/140**, each case by name. 117 cases were there at 1e0a2483; 23 are new. |
| `typecheck.txt`, `wrangler-dry-run.txt` | `pnpm typecheck` and `wrangler deploy --dry-run`; both exit 0. |
| `deploy-gap.txt` | The deployed Worker's `src/` (392f9c09, PR 73; the gateway source of main 54f53be7) on migrations 0001, 0002 and 0003. gp-ac0u's check and this round's (`deploy-gap.check.ts`) both pass. The copies it ran are the two `.ran` files. |
| `mutation-check.txt` | **The mutation check.** gp-ac0u's 74 mutations, re-cut where this round changed the line, plus 44 that each break one gp-0g2s rule. Every one must turn the suite red. Three equivalent mutants are listed apart, each with its reason. **118/118 killed at 43f64947.** |
| `mutation-check-48e53c7d.txt`, `mutation-check-8dab7afb.txt` | The same check on 48e53c7d (113/117) and on 8dab7afb (116/117). Their survivors are what 8dab7afb and 43f64947 answer. |
| `self-review-r1.md` | The fresh reviewer's findings on 735e5373 (1 MAJOR, 4 MINOR, 5 NIT) and the lane's dispositions. |
| `self-review-r1-scenarios.test.ts.txt`, `self-review-r1-crosscheck.txt` | The reviewer's reproductions, and their run on 48e53c7d: the five that reproduced a finding no longer do. |

## The bead's ACCEPTANCE: the four RED tests, by name (`vitest-green.txt`; each fails in `vitest-red-schema.txt`)

1. **PR 76 MAJOR 1 (gateway.ts:897), under the mayor's rule R1.** "a `to` naming two owners of
   the city (one's retained key, the other's current number) answers 409 ambiguous_recipient and
   reaches neither; each owner's current ids reach that owner alone (gp-0g2s, the mayor's R1)".
2. **PR 76 MAJOR 2 (gateway.ts:707), in the gate's order.** "a message sent before the change whose
   first delivery failed, retried after the notice committed, reaches the owner's conversation: no
   canned line, no id changes, its window moves".
3. **PR 73 MAJOR (gateway.ts:575), the bead's sequence.** "after the phone is unbound and joins
   another city, Meta's retry of an acknowledged image reaches no city again and grants the new
   city no media; the joining text likewise; Meta gets 200". Also "a message whose first delivery
   failed, retried after the owner moved to another city, reaches neither city and grants no
   media..." for the order the self-review found.
4. **PR 73 MINOR (gateway.ts:532).** "redeliveries of two wrong-code texts interleaved (A, B, A, B,
   A) are two attempts, and the owner's right code then binds (gp-0g2s)".

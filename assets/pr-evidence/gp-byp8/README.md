# gp-byp8 evidence: WhatsApp gateway round 4 (`whatsapp/gateway/`)

- **What it fixes.** PR 76's codex gate r3 of record on the round-3 delta, `/Users/tailor512/city/assets/ops/codex-gates/gp-w376-pr76-r3-e39c0b904.txt` (05:00Z 11/5): one CRITICAL at `gateway.ts:839`. C, a phone-only owner in city W, holds number P. Meta shows P beside A's user_id at T2, and C's row keeps A's row from taking P. Round 3's guard then recorded nothing for P at A's notice (T4). A's image in today's shape (P alone), stamped T3 and first delivered after T4 while C stands, went to W, media included. The round also folds in two reviews of its own cuts (below).
- **Base.** e39c0b9040605cdb41de9d0d994747c44846b628, branch `feat/gp-ac0u-whatsapp-bsuid`, on main 54f53be7.
- **Commits.**
  - 17a54f6e: the first cut. The paired number (an open span in `phone_aliases`), the conflict as the refusal's reason, and the spec table.
  - d520e098: the codex self-review r1's MAJOR (`codex-self-review-r1-17a54f6e.txt`). Meta's replay of two owners' pairings reopened an ended span, and the next end then failed the payload on the primary key.
  - 3b1ba734: the lane's fresh self-review r1 (`self-review-r1.md`): 2 CRITICAL, a MINOR and 2 NITs, each with its disposition.
  - 22dd1294: the codex self-review r3's MAJOR (`codex-self-review-r3-3b1ba734.txt`). 3b1ba734 found a change's owner by `routeKey` alone. After two changes, Meta's replay of the first notice reached the owner through its previous user_id and rolled it back to its middle ids. The change statements now need both the current holder (`ownerKey`) and the route.
  - 17f86937: the codex self-review r4's CRITICAL (`codex-self-review-r4-22dd1294.txt`). Meta's replay of a pairing after the owner's notice was kept as a second span over the end Meta had recorded. A late pairing now follows the on-time rule: it widens the first span it belongs to, or else keeps a span up to the change.
  - The evidence commit follows. It changes no file under `src/`, `test/` or `migrations/`, so every run here at 17f86937 stands for the head.
- **Toolchain.** `/Users/tailor512/city/.gc/shims/toolchain/pnpm` (pnpm 11.20.0).
- **Headers.** Every transcript names its HEAD and the tree it ran on.
- **Regenerating.** From the worktree root:
  - `python3 -I assets/pr-evidence/gp-byp8/evidence.py "$PWD" <pnpm> <out dir>`
  - `python3 -I assets/pr-evidence/gp-byp8/mutate.py "$PWD/whatsapp/gateway" <pnpm>`. Pass `--check` to only count the patterns, `--only=<text>` for a quick look.

  Each script writes the committed files back and checks them by sha256 against HEAD.

| File | What |
|---|---|
| `vitest-red-base.txt` | **RED A: 37 failed, 156 passed.** The committed tests against e39c0b90's `src/` and 0003. The 37 are this round's. The 6 new tests that pass there pin guards of this round's own changes. |
| `vitest-red-first-cut.txt` | **RED B: 12 failed.** The committed tests against 17a54f6e's `src/`. The 12 are the tests of the two later fixes. |
| `vitest-red-second-cut.txt` | **RED C: 9 failed.** The committed tests against d520e098's `src/`: the tests of the fixes after it. |
| `vitest-red-third-cut.txt` | **RED D: 3 failed.** The committed tests against 3b1ba734's `src/`: the codex self-review r3's and r4's tests. |
| `vitest-red-fourth-cut.txt` | **RED E: 2 failed.** The committed tests against 22dd1294's `src/`: the codex self-review r4's tests. |
| `vitest-green.txt` | **GREEN: 193/193**, each case by name: e39c0b90's 150, unchanged, and 43 new. |
| `typecheck.txt`, `wrangler-dry-run.txt` | `pnpm typecheck` and `wrangler deploy --dry-run`; both exit 0. |
| `deploy-gap.txt` | The deployed Worker's `src/` (392f9c09, PR 73; the gateway source of main 54f53be7) on migrations 0001, 0002 and this round's 0003 (changed in comments only). gp-ac0u's and gp-0g2s's checks both pass. The copies it ran are the two `.ran` files. |
| `mutation-check.txt` | **180/180 killed at 17f86937.** gp-w376's list re-cut where this round changed a line (marked), plus one gp-byp8 mutant per rule. 4 equivalents survive, each with its reason. |
| `mutation-check-17a54f6e.txt`, `mutation-check-d520e098.txt` | The runs at the earlier cuts: 156/156 and 162/162. The run at 3b1ba734 is not kept: the lane edited the test file while it ran, which made it invalid. |
| `codex-self-review-r1-17a54f6e.txt` | The lane's codex self-review through the hub, STANDARD, on `e39c0b90..17a54f6e`: **BLOCK**, 1 MAJOR, fixed in d520e098. |
| `codex-self-review-r2-d520e098.txt` | The same on `e39c0b90..d520e098`: **CLEAN**. The fresh self-review, run beside it on 17a54f6e, still found 2 CRITICALs (fixed in 3b1ba734). |
| `codex-self-review-r3-3b1ba734.txt` | The same on `e39c0b90..3b1ba734`: **BLOCK**, 1 MAJOR, fixed in 22dd1294. |
| `codex-self-review-r4-22dd1294.txt` | The same on `e39c0b90..22dd1294`: **BLOCK**, 1 CRITICAL, fixed in 17f86937. |
| `codex-self-review-r5-17f86937.txt` | The same on `e39c0b90..17f86937`: **BLOCK**, 1 CRITICAL, **open** (below). `codex-r5-repro.test.ts.txt` reproduces it at 17f86937. |
| `self-review-r1.md` | The fresh reviewer's findings on 17a54f6e, and the lane's disposition of each. |
| `probe-rule3-unbound-owner.test.ts.txt` | The lane's probe of a rule-3 limit; now fixed (see `self-review-r1.md`). |

## The bead's ACCEPTANCE: the RED test, by name

**SCOPE 1 reproduced.** `test/bsuid.test.ts`, "the codex gate r3's sequence: Meta pairs P with A's user_id by a message while C's phone-only binding holds P; A's image stamped after the pairing, delivered after A's notice while C stands, goes to nobody (gp-byp8)". On e39c0b90 it fails three ways (`vitest-red-base.txt`): W's inbox holds A's image (`expected true to be false`), W's `GET /v1/media/<id>` answers 200 (`expected 200 to be 404`), and nothing is counted (`expected { unrouted: +0, conflicts: +0, … } to match object`).

It is the first row of a table, the round's spec: 2 ways Meta pairs P with A (a message beside A's user_id; A's number change) x 8 rows.

| Delivered | Others | A's image (stamped T3) | C's text (stamped T1) |
|---|---|---|---|
| after A's notice | C stands | nobody, counted as a conflict; no media | C's |
| after A's notice, which names no old number | C stands | nobody, counted as a conflict | C's |
| before A's notice | C stands | nobody, counted as a conflict | C's |
| before A's notice | C stands and A is unbound | nobody, counted as a conflict | C's |
| after A's notice | C is unbound | A's, media to X only | a stranger's |
| after A's notice, which names no old number | C is unbound | A's | a stranger's |
| before A's notice | C is unbound | A's | a stranger's |
| before A's notice | C is unbound, then A shows P again | A's | nobody's, counted |

**What the table checks.** In every row, W gets neither the image nor its media. Rows with "C stands" and A's notice also check that P alone, stamped after the notice, is C's again.

**The targeted tests.** 27 follow the table, each the one that kills a mutant of its rule. `mutate.py` names the rule each one breaks.

**The 6 tests that pass on e39c0b90.** Each pins a guard of this round's own change:
- a pairing landing after C bound pairs nothing (2);
- a row holding the number is no paired holder of it;
- a late pairing stamped before the owner bound keeps nothing from before the bind;
- the rule-3 doubt only refuses;
- a replayed notice never rolls the owner back (the codex self-review r3).

## OPEN: the codex self-review r5 CRITICAL, and why the round stopped

**The sequence.**
1. A's first pairing (P beside U) at T1 is delayed until after A's notice at T4.
2. Meanwhile P beside V at T2 gave phone-only C the user_id V. Rule 3 records no stamp for that.
3. The late pairing is kept as A's span [T1, T4].
4. Once C is unbound, C's own image stamped T3 reaches X, and X is granted its media.

`codex-r5-repro.test.ts.txt` fails at 17f86937 with exactly that. e39c0b90 does not route the image to X: this is a regression of 3b1ba734's late-pairing history. Removing that history brings back the fresh self-review's CRITICAL 1.

**Why the round stopped.** The codex self-reviews r1, r3, r4 and r5, and the fresh review, each found the next delivery order of one problem. The gateway rebuilds spans from evidence that arrives out of order, but records only some of it: rule 3's user_id fill has no stamp. Under the mayor's rule after repeated rounds on one predicate, the round stops here with a design question (the STOP mail on the bead): late evidence only refuses, or a pairing ledger. PR 76 stays at e39c0b90, and this work is on fork2 branch `gp-byp8-wip`.

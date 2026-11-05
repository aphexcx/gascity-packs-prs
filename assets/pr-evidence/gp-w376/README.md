# gp-w376 evidence: WhatsApp gateway round 3 (`whatsapp/gateway/`)

- **What it fixes.** Two reads of PR 76's round-2 head ba82ab46, and the lane's own self-review:
  - The codex gate r2 of record on the round-2 delta: `/Users/tailor512/city/assets/ops/codex-gates/gp-ac0u-pr76-r2-ba82ab461.txt`. It found a MAJOR (gateway.ts:823) and a MINOR (0003_retries.sql:45).
  - The Fable read r1 of the whole PR: `/Users/tailor512/city/assets/ops/codex-gates/gp-ac0u-pr76-fable-read-r1-ba82ab46.md`. It found 1 CRITICAL (the gate's MAJOR), 2 MINOR (the gate's MINOR, and the 409 body) and 5 NOTEs. The bead folds it in as ADDENDUM 1.
  - The lane's fresh self-review of `ba82ab46..eaf93d2b` (`self-review-r1.md`): 1 CRITICAL (the alias span, when another row held the number every time a message could have given it to the owner), 2 MINOR and 2 NIT.
- **Base.** ba82ab461d4767a3b9b652991446d748741e8f5b, branch `feat/gp-ac0u-whatsapp-bsuid`, on main 54f53be7.
- **Commits.**
  - bc99bac1: the alias span (the gate's MAJOR / the Fable read's CRITICAL).
  - e6e5b866: the dedup backfill (the gate's MINOR / the Fable read's MINOR 1).
  - 64fff96d: the Fable read's MINOR 2 (the 409's `owner_user_id`) and NOTEs 4, 5 and 6.
  - eaf93d2b: the mutation check's one survivor, answered by a fifth alias-table row.
  - 1d85f5ba: the self-review's CRITICAL (a number the records never gave the owner is no previous holding of it), its NIT test, and the mutation survivor that followed.
  - The evidence commit follows. It changes no file under `src/`, `test/` or `migrations/`, so every run here at 1d85f5ba stands for the head.
- **Toolchain.** `/Users/tailor512/city/.gc/shims/toolchain/pnpm` (pnpm 11.20.0).
- **Headers.** Every transcript names its HEAD and the tree it ran on.
- **Regenerating.** From the worktree root:
  - `python3 -I assets/pr-evidence/gp-w376/evidence.py "$PWD" <pnpm> <out dir>`
  - `python3 -I assets/pr-evidence/gp-w376/mutate.py "$PWD/whatsapp/gateway" <pnpm>`. Pass `--check` to only count the patterns, `--only=<text>` for a quick look.

  Each script writes the committed files back and checks them by sha256 against HEAD.

| File | What |
|---|---|
| `vitest-red-base.txt` | **RED A: 10 failed, 140 passed.** The committed tests against ba82ab46's `src/` and 0003. The 10 are this round's: the five alias-table rows, the two "never gave it" tests, the 0003 backfill test and the two 409 tests. |
| `vitest-red-addendum.txt` | **RED B: 9 failed, 141 passed.** The committed tests against e6e5b866's `src/`, where the gate's two findings are fixed but the Fable read's others and the self-review's are not. The 9 are the two 409 tests, the five rows' log-line check and the two "never gave it" tests. |
| `vitest-red-self-review.txt` | **RED C: 2 failed, 148 passed.** The committed tests against eaf93d2b's `src/`, the head the self-review read. The 2 are the "never gave it" tests. |
| `vitest-partial-823-only.txt` | **1 failed, 143 passed.** bc99bac1's tests (the first four rows) against ba82ab46's `src/`, with line 823 alone changed. The row where another row held the number still fails, so the re-key's own change is needed too. |
| `vitest-green.txt` | **GREEN: 150/150**, each case by name. 140 were there at ba82ab46 (one, the R1 409 test, now pins the new body), and 10 are new. |
| `typecheck.txt`, `wrangler-dry-run.txt` | `pnpm typecheck` and `wrangler deploy --dry-run`; both exit 0. |
| `deploy-gap.txt` | The deployed Worker's `src/` (392f9c09, PR 73; the gateway source of main 54f53be7) on migrations 0001, 0002 and this round's 0003. gp-ac0u's and gp-0g2s's checks both pass. The copies it ran are the two `.ran` files. |
| `mutation-check.txt` | **131/131 killed at 1d85f5ba.** That is gp-0g2s's 118 (five re-cut where this round changed the line) plus 13 that each break one gp-w376 rule. gp-0g2s's three equivalents are listed apart and still survive, each with its reason. `mutation-check-eaf93d2b.txt` is the 127-mutation run at eaf93d2b (127/127), before the self-review fix. |
| `codex-self-review-r1-eaf93d2b.txt`, `codex-self-review-r2-1d85f5ba.txt` | The lane's codex self-reviews through the hub, STANDARD (from the bead), on `ba82ab46..eaf93d2b` and on `ba82ab46..1d85f5ba`: **both CLEAN**. |
| `self-review-r1.md` | The fresh reviewer's findings on eaf93d2b, and the lane's disposition of each. |

## The bead's ACCEPTANCE: the RED tests, by name (each fails in `vitest-red-base.txt` and passes in `vitest-green.txt`)

1. **The alias span (gate r2 MAJOR, gateway.ts:823; Fable read CRITICAL 1)**, `test/bsuid.test.ts`, five rows of "a number A's records gave it after its bind is A's previous holding from then, never from the bind (gp-w376): ...":
   - "the gate's sequence: a number change gives A the number P at T2, and B takes P at T3"
   - "a message shows P beside A's user_id at T2 (one owner, two ids, one row), and B takes P at T3"
   - "a number change names P at T2 while C, a phone-only owner bound at T1 + 30 min, holds P, so A's row never takes it"
   - "a number change at T2 shares no number, so A's row holds none; B takes P at T3, and A's next notice names P as A's old number (the mutation check's survivor)"
   - "the Fable read r1's reproduction: P was C's, bound in a third city at T0 and unbound after T1; a message shows P beside A's user_id at T2; B takes P at T3"

   In each row, a phone-only text and image stamped T1 reach no city, and the image's media id resolves for no city (404 for every city in the test). The text and image stamped T2 + 30 min go where the spans send them, media included.

   The self-review's two tests, "a number the owner's records never gave it (another row held it when a message showed it beside the owner's user_id | throughout) is no previous holding of the owner: what that row sent reaches its own city while it stands, and no city once it is unbound (gp-w376)", add the case where another row held the number every time a message could have given it to the owner.
2. **The dedup backfill (gate r2 MINOR, 0003_retries.sql:45; Fable read MINOR 1)**, `test/bsuid.test.ts`: "0003 carries each stranger's last counted code text into the retry dedup (gp-w376): a fourth wrong code replayed after the deploy is still the fourth, so a right code as the fifth binds; another stranger's fifth counts five and its sixth is refused". On ba82ab46 it fails with "expected 5 to be 4".
3. **The 409 body (Fable read MINOR 2)**, `test/bsuid.test.ts`: the R1 test "a `to` naming two owners of the city ... (gp-0g2s, the mayor's R1)", which now pins `owner_user_id`, and "the 409's owner_user_id is the owner `to` keys whatever order D1 reads the two rows in".

One test is not RED-first. "a number another owner's change takes from its holder (rule 2) stays the holder's previous holding from the holder's bind ..." pins behaviour that ba82ab46 and eaf93d2b already had. 1d85f5ba's guard would drop that behaviour if a change's strip stopped keeping the number's start; the test kills that mutant.

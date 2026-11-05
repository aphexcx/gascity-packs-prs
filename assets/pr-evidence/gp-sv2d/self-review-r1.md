# gp-sv2d fresh self-review r1: findings and dispositions

- **Reviewer.** One Opus 5.5 subagent, the lane's own model. It was given the bead (gp-sv2d), the
  delta `e39c0b90..feeca22a` (the whole round-4 delta) and the gate records (gate r3, the Fable read
  r2, the codex self-review r5 and its repro, gp-byp8's `self-review-r1.md`), but not the lane's
  reasoning. It worked in its own detached worktrees under `/private/tmp/gp-sv2d-review/` (feeca22a,
  e39c0b90 and 17f86937), left in place.
- **Verdict.** BLOCKER 0, CRITICAL 4, MAJOR 2, MINOR 1, NIT 1. Every finding is reproduced by a
  test: `review-r1-repro.test.ts.txt` (14 tests: 13 repros and one control).
- **Checked by the lane at the round's last code commit.** `review-r1-repro-at-head.txt` is that file
  run on 082f5f3b's `src/` and 0003, which are byte-identical to 159f53c5's: 13 failed, 1 passed (the
  control).
- **The round stops on it** (`README.md`, "Why the round stops").

## Findings

| # | Severity | Finding (the reviewer's words, condensed) | Repros | vs e39c0b90 | Disposition |
|---|---|---|---|---|---|
| 1 | CRITICAL | Another binding's late pairing is treated as on-time evidence: it cuts spans (`pairingEnds`) and opens routing spans (`pairingOpens`). An open span records only its start, so later on-time pairings leave no trace. A late pairing from another binding is neither (A)'s late (i) nor its late (ii). | R1, R2, R2b (regressions); R7 (the base too: W while C stands, the gate r3 harm) | regression (gp-byp8's commits; 17f86937 the same) | **OPEN: the design question.** Fixing it needs a record of how late each pairing is, beyond (A)'s two structural kinds. |
| 2 | CRITICAL | `pairingOpens` starts a span at `wa_since` even when that stamp starts a different number: rule 2's strip keeps the old number's start, and the re-key writes the stamp of a change that named another number. | R3, R3c (X with media once C is unbound); R3b (C's own image refused while C stands; the base gives it to W) | regression (gp-byp8) | **OPEN.** A one-line fix (start at the pairing's stamp) drops gp-byp8's "paired number from the change" rule and the pin 159f53c5 added for it. Left for the mayor's ruling with finding 1. |
| 3 | CRITICAL | `refusedCode` reads the code's row now, not at the text's stamp. `POST /v1/invites` deletes a city's unspent code when it mints a new one. | R4 (re-mint, then the retry is adopted into W, and the next live code reaches W), R4b (another phone spent it), R8 | the base adopts any text | **OPEN: the bead's stop rule** ("it never ships adoption into W"). A fix keeps re-minted codes as revoked rows and tests liveness at the stamp. That changes PR 73's invite endpoint. |
| 4 | CRITICAL | A mistyped code (or any text) before the live one: rule 3 makes C adopt a, and X's live code then reaches W unspent. | R9 | the base too | **OPEN: a spec conflict.** SCOPE 2(c) keeps adoption "for a message that is no live invite code", which collides with its "never". The mayor's ruling on rule 3. |
| 5 | MAJOR | The fill writes `wa_since` = a late message's stamp, so late evidence still routes through the current-holder branch. | R6 | the base too | **OPEN**, the same class as finding 1. |
| 6 | MAJOR | feeca22a did not touch README.md: rule 5 contradicted the code, and the refuse-only rule, MINOR 4's answer and the rollback caveat were missing. | — | — | **FIXED in 082f5f3b** (README rules 3, 5 and 6; Deploy step 3). |
| 7 | MINOR | A refused invite text beside a phone-only holder (the attempts used up) reaches nobody, as it should, but is not counted: `routeKey` resolves to C, and the pre-read marks the sender as owned. | R5 | the base delivered it to W | **OPEN**, left with the round. |
| 8 | NIT | gateway.ts cites README rule 6 for the stranger definition. | — | — | **ANSWERED by 082f5f3b**: rule 6 now carries "Who hears the canned line" (MINOR 4). |

## What the reviewer found sound

- No primary-key collision that answers 500 forever: every `phone_aliases` write is `INSERT OR
  IGNORE`, `UPDATE OR REPLACE`, or touches only `since`.
- No error in how the log line counts conflicts, beyond finding 7.
- The round's own tests prove what their titles say.

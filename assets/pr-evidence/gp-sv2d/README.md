# gp-sv2d evidence: WhatsApp gateway round 4b (`whatsapp/gateway/`)

- **What it built.** Afik's shape (A), 11:20:11Z Thu 11/5: "A. and the above prs are approved"
  (`/Users/tailor512/city/assets/records/founder-words/afik-20261105-112011Z-a-and-the-above-prs-are-approved.md`).
  Late evidence refuses, never routes. The round also built the invite path beside a phone-only
  holder (the Fable read r2 CRITICAL 2), the mirrors, the 7-day wording and the rollback caveat.
- **Where it stopped.** The round's fresh self-review r1 found 4 CRITICALs, every one reproduced by
  a test (below). It stops on the bead's own rule for the invite path ("it never ships adoption into
  W") and on the mayor's rule after repeated rounds on one predicate. PR 76 stays at e39c0b90 (a
  DRAFT). The work is on fork2 branch `gp-sv2d-wip`.
- **Base.** gp-byp8-wip c5c48f3b (code head 17f86937), on PR 76's e39c0b90.
- **Commits.**
  - feeca22a: the marker, routeKey's exclusion, the invite path, the 7-day wording, and the tests.
  - 082f5f3b: README (rules 3, 5 and 6, Deploy's rollback). The bind keys as before. Two tests.
  - 159f53c5: two tests the mutation probe found missing.
  - The evidence commit follows. It changes no file under `src/`, `test/` or `migrations/`.
- **Toolchain.** `/Users/tailor512/city/.gc/shims/toolchain/pnpm` (pnpm 11.20.0).
- **Regenerating.** From the worktree root:
  - `python3 -I assets/pr-evidence/gp-sv2d/evidence.py "$PWD" <pnpm> <out dir>`
  - `python3 -I assets/pr-evidence/gp-sv2d/mutate.py <gateway dir> <pnpm>` (never on a tree being
    edited). `--check` only counts the patterns; `--only=<text>` runs a few.

## What the round built

- **The marker** (0003 is unmerged, so it is a column there). `phone_aliases.routes_from`: a span
  holds `[since, changed_at]` and routes `[routes_from, changed_at]`.
  - An on-time span routes from its start: `pairingOpens` and the change's previous ids.
  - A pairing under a previous user_id (`pairingHistory`, late (i)) is kept with `routes_from`
    253402300799, so it never routes.
  - A pairing stamped before a span's start (`pairingWidens` and `pairingHistoryWidens`, late (ii))
    moves `since` back and leaves `routes_from` where it was.
  - Every end's merge (`ended`) keeps the earlier of each column: the exact union of the two spans.

  It is a stamp, not a flag. A flag needs a second row for each late widening, and a fourth column
  in the primary key. The stamp keeps every one of gp-byp8's span assertions valid.
- **routeKey** joins only a holding that routes the stamp (`holdings` carries `routes`). A stamp that
  only a refuse-only part holds goes to nobody, counted; one that another binding holds too is a
  conflict.
- **The invite path.** A text from a user_id no row holds, beside a number a phone-only row holds,
  may carry an invite code (`mayBind`).
  - Its attempt is counted as a newcomer's (`newcomer`).
  - The bind keys the new row by the user_id with no `wa_id`. `pairingOpens` makes the number its
    paired number, so the number is contested.
  - A newcomer's live code whose bind was refused is no one's message (`refusedCode`).
  - Any other text keeps rule 3.
- **README.** Rule 3's invite exception; rule 5's refuse-only bullet with (A) and its fail-closed
  cost; MINOR 3 (why a phone-only holder and one with its own user_id differ); rule 6 (who hears the
  canned line, MINOR 4); an end only gives stamps back; Deploy's rollback caveat (NOTE 6). The 7-day
  retry window is named an assumption wherever the code states it (NOTE 8).

## The record

| File | What |
|---|---|
| `vitest-green.txt` | **GREEN 215/215** at 159f53c5: gp-byp8's 193 and 22 new. |
| `vitest-red-byp8-head.txt` | **RED A: 10 failed** with 17f86937's `src/` and 0003. They are the codex self-review r5's sequence (SCOPE 2(a)), the invite path (SCOPE 2(c): reproduced, the code text lands in W and C adopts a), the four refuse-only rows with C unbound, the marker, the refused live code, and the two merge tests (the new column). |
| `vitest-red-base.txt` | **RED B: 56 failed** with e39c0b90's `src/` and 0003: the whole round 4's tests. |
| `typecheck.txt`, `wrangler-dry-run.txt` | Both exit 0. |
| `deploy-gap.txt` | PR 73's `src/` (392f9c09) on 0001, 0002 and this 0003 passes gp-ac0u's and gp-0g2s's checks (`.ran` copies). |
| `mutation-probe-082f5f3b.txt` | The probe run: **198/200** killed, 6 equivalents survive with their reasons. |
| `mutation-survivors-159f53c5.txt` | The probe's two survivors, both killed by 159f53c5's two tests. |
| `mutation-check.txt` | **200/200 killed** on 159f53c5's `src/` and tests. The list is gp-byp8's 180, re-cut where this round changed a line, plus 20 gp-sv2d mutants, one per rule. 6 equivalents survive, each with its reason, two of them this round's (the merge's `routes_from` MIN, and the rule-3 doubt's `routes` 0). |
| `self-review-r1.md` | The fresh self-review r1, each finding with its disposition. |
| `review-r1-repro.test.ts.txt` | The reviewer's 13 repros and its control (not in the suite: they fail at the head). |
| `review-r1-repro-at-head.txt` | That file run at this head's `src/`: 13 failed, 1 passed (the control). |
| `codex-self-review-r1-159f53c5.txt` | The lane's codex self-review through the hub (CODEX_HOME `/Users/tailor512/.config/citadel-mayor/codex-hub`), STANDARD, on the round delta `c5c48f3b..159f53c5`: **BLOCK, 2 MAJOR**. (1) A late pairing's retry fills A's `wa_id` from its stamp, so the current holding routes a refuse-only interval: the fresh review's finding 5. (2) A refused invite text, replayed after its code was spent or replaced, is adopted by C: finding 3. Both open, with the stop. |

## Why the round stops

- **The invite path still ships adoption into W** (findings 3 and 4), and the bead says it stops
  then.
  - R4, R4b and R8: Meta's retry of an invite text after the code was re-minted (`POST /v1/invites`
    deletes the unspent code) or spent. The code reads as no live code, so C adopts a, and a's next,
    live code reaches W.
  - R9: a mistyped code, or any text, before the live one. Rule 3 adopts a first. SCOPE 2(c) keeps
    adoption "for a message that is no live invite code", which collides with its "never".
- **(A)'s two kinds of late do not cover the class** (findings 1, 2 and 5). Each of these is the
  next delivery order of the problem gp-byp8 stopped on: the gateway rebuilds spans from evidence
  that arrives out of order and records only part of it.
  - R1, R2 and R2b: another binding's pairing, delivered late. These regress against e39c0b90.
  - R3 and R3c: a span start taken from `wa_since` of another number. These regress too.
  - R7 and R6: the base has these as well.
  - The codex self-review r1 found the same two classes from its own sequences: a late fill, and a
    refused invite replayed after its code changed.

  A fourth patch would be a third round of predicates on the same rule.

## The design questions (the STOP mail on the bead)

- **Q1: what "late" means.**
  - (a) A per-number high-water mark: the newest pairing stamp the gateway has processed for each
    number. A pairing stamped below it is late, and only refuses. It strips nothing, fills nothing,
    ends no span and opens no routing span. This is one small table, a narrow (B).
  - (b) The gateway's clock: evidence stamped more than a grace before its arrival is late. No new
    state, but the clocks mix and the grace is a guess.
  - (c) Revert the paired-number machinery to e39c0b90, with only the gate r3 guard.
  - With (a), R3 also needs the paired span to start at the pairing's stamp, not at `wa_since`.
- **Q2: rule 3 and invite codes (R9).** (a) A six-digit text from a user_id no row holds is never
  adopted: a mistyped code goes to nobody, and a live one binds. (b) Keep rule 3 as is, and accept
  that the invite path covers only a newcomer's first text.
- **Q3: the code's lifecycle (R4, R4b, R8).** Keep re-minted and spent codes with the stamp they
  stopped, and test liveness at the text's stamp. This changes PR 73's invite endpoint.

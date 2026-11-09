# gp-0g2s self-review r1: 735e5373 (delta 1e0a2483..735e5373)

Reviewer: an adversarial self-review subagent (fresh context). The worktree was not modified.
Scratch copies: /private/tmp/gp-0g2s-review (scenarios) and /private/tmp/gp-0g2s-review-mut
(mutation probe). Baseline at 735e5373: `vitest run` gives 131 of 131 passing.

Reproductions are in `/private/tmp/gp-0g2s-review/whatsapp/gateway/test/review.test.ts`. Each
test asserts what the code does NOW, so a green run is the reproduction: 9 of 9 pass. The
mutation probe is `/private/tmp/gp-0g2s-review/mutate_review.py`; its output is in
`mutation-review.txt` and `mutation-review-2.txt` in the same folder.

Counts: 0 BLOCKER, 1 MAJOR, 4 MINOR, 5 NIT.

---

## MAJOR 1: a never-stored wamid still follows the phone to the next city (the PR 73 MAJOR in the order where the first delivery failed); an alias whose binding is gone falls through to the current holder

**Where:** src/gateway.ts:281 (`routeKey` = COALESCE(aliasKey, ownerKey)), :695-704 (the
message insert), :705-716 (the media grant follows that row). README.md:209-210 ("The
binding ... it names nobody") and :503-505 ("so nothing reaches the next city").

**The fix covers only a wamid some city already holds.** The new dedup key is
UNIQUE(wamid, status), and media follows the stored row. Meta retries mostly because the
gateway did not commit: a D1 failure answers 500. In that case no city holds the wamid. The retry
is then routed by the ids as they stand when it arrives. The PR 73 gate wrote "a late Meta retry
routes its earlier message to B", and that is still true for this order.

**S1a (reproduced; the README's documented procedure, the same owner):**
1. Owner (P, a) is bound to city 1.
2. At T0+1h it sends a text and an image. Each webhook answers 500 (flakyDb).
3. The operator unbinds it (README "Unbind an owner"). It joins city 2 with the same (P, a).
4. Meta retries both.
5. City 2's inbox receives both of city 1's messages, and `GET /v1/media/<id>` answers 200 for
   city 2.

**S1b (reproduced; another person, today's shape):**
1. P is bound phone-only in city 1. A's text answers 500.
2. The operator unbinds P. A different person binds P phone-only in city 2.
3. A's retry is delivered to city 2.

**S2 (reproduced; the alias rule this commit adds):**
1. A (P, a) is in city 1. A's phone-only text at T0+1h answers 500.
2. A's change notice commits at T0+2h (aliases P and a).
3. The operator unbinds A. C joins city 2 as (P, c).
4. The phone-only retry arrives. aliasKey picks A's alias, finds A's binding gone, and returns
   NULL. routeKey falls through to `ownerKey(P, NULL)`, which is C.
5. The message is delivered to city 2 under C's conversation. The plain UPDATE also writes A's
   old profile name onto C's row.

The alias "names nobody", but the message does not go nowhere. The README's sentence "nothing
reaches the next city" is false here. The test at bsuid.test.ts:753 covers only the BSUID shape
(P, a). There, ownerKey cannot match C, because C's user_id differs.

**Fix direction (the stamp decides, applied to current ids too):**
- A binding routes, by its current ids, only what Meta stamped at or after the stamp of its own
  bind text. Store that stamp on phones at the bind, for example `bound_stamp`, and add
  `AND ?at >= COALESCE(bound_stamp, 0)` to the current-id route, the plain UPDATE and the stranger
  test.
- A message stamped before every binding that could take it is dropped and counted. It gets no
  canned line.
- Separately, when an alias row exists for the ids at or after the stamp (changed_at >= at),
  routeKey must not fall back to ownerKey. If the alias's binding is gone, the answer is nobody.
- Test both shapes and both orders (stored first, failed first). Also correct the two README
  sentences.

## MINOR 1: a late number-change notice strips the recycled number from its new holder, and the alias then records no old number

**Where:** src/gateway.ts:615-629 (the stale-number strip runs for a change notice too) and
:756-771 (the alias insert records the row's wa_id, never the notice's `from`).

**The notice's own ids are history.** A change notice names (from = old number P,
previous_user_id = a). That pairing is history by definition. The strip's new guard,
`NOT EXISTS phone_aliases WHERE id = ?2`, does not cover it on the notice's first landing: the
alias of a is written later in the same group.

**S3 (reproduced):**
1. A (P, a) is bound. A's phone-only text answers 500.
2. A's notice (P, a → Q, a2) answers 500.
3. B, the new holder of recycled P, joins the same city as (P, b). B's join strips P from A.
4. The notice lands. Its strip removes P from B, because B's user_id b <> a and no alias of a
   exists yet. Its plain UPDATE then refills A's wa_id with P, and the re-key moves A to (Q, a2).
5. B is left with no number. B's next text in today's shape (P alone) is a stranger's: it gets
   the canned line and is not delivered.

**Fixing only the strip is not enough.** The obvious fix is to skip the strip when `m.change` is
set. But then A's row lacks P when the alias insert runs. No P alias is written, and A's
phone-only pre-change retry routes to B. So the alias insert should also record the notice's own
`from` and its previous_user_id, besides the row's ids. This has the same plausibility class as
the gate's MAJOR 1: recycling within Meta's 7-day retry window.

## MINOR 2: a redelivered notice can re-key a phone-only holder of the old number onto the owner's old ids

**Where:** src/gateway.ts:748-792. The change block has no `aliasKey(...) IS NULL` guard. The
plain UPDATE at :744 gained that guard in this commit.

**S4 (reproduced):**
1. A is a username owner (conversation_id a) who later shows phone P.
2. Notice 1 (P, a → Q, a2) commits.
3. B binds P phone-only in another city. P is nobody's conversation_id, so B's conversation_id
   is P.
4. A's second notice (Q, a2 → R, a3) commits.
5. Meta redelivers notice 1. `ownerKey(P, a)` matches B through the `user_id IS NULL` arm.
   Nothing else holds a2 now, so the re-key gives B wa_id Q and user_id a2.
6. B's city sends a template to B's conversation P. It goes to Q, A's former number.

**Fix:** guard the alias insert, the new-number strip and the re-key with
`aliasKey(?3, ?4, at) IS NULL`. A notice whose ids are already an alias is a redelivery. This
path existed before this commit, at 1e0a2483. It falls under this round's own "a notice
redelivered" rule, and the guard costs one line.

## MINOR 3: README rule 7's resend advice is wrong in exactly the situation that produces the 409

**Where:** README.md:233-236 ("the `user_id` of its newest delivery, or of `GET /v1/window`").

**S6 (reproduced):**
1. A changes number, and B joins the same city on P.
2. `to: P` answers 409, as intended.
3. A's newest delivery is the change notice itself. Its `user_id` is the OLD id a: the message
   insert stores `COALESCE(?4, user_id)`, and ?4 is previous_user_id.
4. Resending to it answers `403 not_bound`. An alias-routed retry also carries the old ids.

Only `GET /v1/window` gives the current id. Fix: the advice should name only `/v1/window`, or the
newest delivery's id once the notice has passed.

## MINOR 4: rules the suite does not pin (mutations that survive all 131 tests)

| Mutation (src/gateway.ts) | Survives? | What breaks |
|---|---|---|
| M4: drop `ORDER BY changed_at` in aliasKey (:274) | Yes | bsuid.test.ts:706 passes only because A's bind wamid sorts before B's. The PK order (id, bound_wamid, changed_at) happens to give the right row. S8 in review.test.ts gives the earlier binding a `wamid.ZZZ…` bind wamid and the later one `wamid.AAA…`, and that kills M4. |
| M1: alias-window UPDATE without `MAX` (:723) | Yes | An old retry rewinds the window after the owner wrote under its new ids. The send then answers 422 window_closed while Meta's window is open. S7 kills M1. |
| M16: the strip guard keyed on `?1` (wa) instead of `?2` (user) (:625) | Yes | The only test (bsuid.test.ts:668) uses a P that is also an alias, so either key blocks the strip. The rule "a user_id an alias holds is history" is untested. |
| M2: alias-window UPDATE pushed for every type (:717) | Yes | Today's code is right. No test shows that a redelivered system notice routed by its own alias leaves the window alone. |

Seven of the commit's own rules are killed by the suite, and M5 too (I tried 20 mutations in
all):
- M5: the binding JOIN
- M17: the plain-UPDATE guard
- M18: the bind guard
- M19: the strip guard
- M20: routeKey order
- M11 and M12: stranger() in the canned claim and in the code count
- M13 and M14: LIMIT 2 and the 409 hint
- M10: code_attempt_wamids
- M15: the alias cron boundary

The other survivors are harmless, and I mention them only for completeness:
- M3: `>=` versus `>` on changed_at (an edge of the same second)
- M6: the media `status = ''` filter
- M7: media received_at taken from now
- M8 and M9: `IS NOT ?1` and `IS NOT ?2` in the alias insert

## NIT 1: a retry with no timestamp is never alias-routed (lost with a 200)

**Where:** :611 and :274. `at = m.timestamp ?? now`, and `changed_at >= now` is never true for a
retry. S5 reproduces it: the retry is a stranger's, gets the canned line to the old number, and
is not stored. Meta always stamps its messages, but the stale filter at :544 deliberately keeps
null-stamped messages. Either document this, or treat a null-stamped message under alias ids as
the alias owner's.

## NIT 2: clock-clamp asymmetry

The notice's changed_at is clamped to the gateway's clock on arrival (`min(ts, now)`). A
retry's stamp is clamped at its own, later arrival. If Meta's clock is ahead by d seconds, a
message the owner sent less than d seconds before the change is not alias-routed. This is
seconds-scale only. Compare Meta's raw stamps, or clamp both against the notice's arrival.

## NIT 3: migration 0003 drops the dedup index before it builds the new one

**Where:** migrations/0003_retries.sql:12-13. If `wrangler d1 migrations apply` does not run the
file atomically and the CREATE fails, deliveries are left with no unique index at all. Every
`INSERT OR IGNORE` then inserts, and every redelivery becomes a duplicate. There is no exposure
today, since the deployed D1 holds no deliveries. Building first under a new name, for example
`deliveries_once_v3`, and then dropping the old index keeps the old dedup on a failure. Nothing
reads the index by name.

## NIT 4: the README cites evidence that is not in the commit

README.md:364 cites `assets/pr-evidence/gp-0g2s/deploy-gap.txt`. At 735e5373 that path does not
exist. The worktree has an untracked `assets/pr-evidence/gp-0g2s/` with deploy-gap.check.ts,
evidence.py and mutate.py, but no deploy-gap.txt. It is presumably pending in the lane's
evidence commit; land it before the gate.

## NIT 5: for the PR body (outside this delta's code)

- **The adapter has no case for the new 409.** PR 74's adapter (b7474ae2, outbound.js:220-257)
  maps `409 ambiguous_recipient` to `permanent / gateway_refused` with no hint. It always sends to
  `conversation_id`, at outbound.js:372 and keeper.js:381.
- **Under R1, the box cannot reach A at all.** From the moment B binds on P in A's city, every
  reply to A and every keeper nudge answers 409. That lasts until the adapter maps a conversation
  to its current id through `/v1/window`.
- **The gate may flag this.** It is the mayor's decided rule, so I am not raising it as a finding.
  The PR body should name it as an open consequence for gp-9hf1, and the readings rule asks for a
  comment on both beads.
- **The pending cap does not see alias-routed messages.** Its pre-read (:569-599) uses current
  ids only, so an alias-routed message's city is not checked against `MAX_PENDING`. That is a
  bounded overshoot.

---

## Areas I examined closely and found clean

- **Parameter numbering.** I checked every `.bind()` in the delta against the ?N each SQL
  fragment uses:
  - :646 count (?5/?6/?7)
  - :668 bind (?1/?2/?7)
  - :703 message (?3/?4/?8)
  - :726 alias-window (?2/?3/?1)
  - :746 plain UPDATE (?5/?6/?4)
  - :770 alias insert (?1..?6)
  - :804 canned (?4/?5/?6)

  Every statement is exercised by a test. My first versions of M11 and M12 left one bound
  parameter unused, and 86 to 88 tests failed, so a numbering slip does not pass quietly.
- **NULL semantics.**
  - aliasKey's `(id = user OR (id = wa AND user IS NULL))` matches by user_id alone when the
    message names one, and by wa_id when it does not.
  - ownerMatch and `stranger` are unchanged in their NULL handling.
  - `at` is never NULL.
  - `user_id IS NOT ?1` and `wa_id IS NOT ?2` behave correctly with a NULL new wa.
- **Name resolution.** The unqualified `id`, `conversation_id` and `changed_at` inside aliasKey
  bind to phone_aliases, even inside the bind insert, whose outer derived table `k` also has an
  `id` column. SQLite resolves to the innermost scope.
- **Upsert parsing.** `INSERT ... SELECT ... WHERE ... ON CONFLICT` has a WHERE, so the upsert
  parse ambiguity does not arise. When the SELECT yields no row, no DO UPDATE runs.
- **Correlated subqueries.** The re-key's `o.conversation_id <> phones.conversation_id` is
  correlated to the updated row, as intended.
- **UNION ALL.** The UNION ALL in the alias insert's FROM yields nothing when ownerKey is NULL.
- **UNIQUE(wamid, status).**
  - A joined row and its message row share (wamid, ''), as before.
  - A status is one send's, of one city.
  - Orphan adoption is unaffected.
  - The media and read lookups use the new index's wamid prefix.
  - I found no path that writes two rows for one inbound wamid (no duplicate delivery).
- **The PR 73 MINOR is fixed.** A, B, A, B, A counts two. A wamid twice in one payload counts
  once. The day reset is intact. The 8-day retention is at least the stale filter's horizon. The
  record-after-count order is pinned by test (M10 is killed).
- **The PR 76 MAJOR 1 (R1) is implemented as the mayor ruled.**
  - With UNIQUE wa_id and user_id, and a BSUID never all digits, the only possible two-row shape
    is a retained conversation_id plus another owner's current wa_id.
  - The query is scoped to the city, so there is no cross-city oracle.
  - The 409 body and the log carry no identifier, and no key is spent.
  - A repeated key still answers its first message_id.
  - Under a race between the read and the claim, the code can only send to A's current id, never
    to P.
- **The PR 76 MAJOR 2 is fixed for the order the gate named.** The bead's three-step order (S0 in
  bsuid.test.ts:589) and its variants pass. They include two owners leaving one number, a
  phone-only retry while another person holds the number, and the bind and canned guards. MAJOR 1
  above is about bindings that are gone, not this.
- **Batch splitting.** When a payload's later batch fails, the redelivery re-runs the
  already-committed notices. Their strip, plain UPDATE, alias insert and re-key all become no-ops,
  through the alias guard and ownerKey returning NULL. Two changes in one payload, in order, chain
  correctly; the reverse order is the README's known limit.
- **The deploy gap.** I read the code; I did not run it. 0003 keeps `last_code_wamid`, so the
  PR 73 Worker's writes still work. The index keeps its name. The two new tables are never read
  by the old code. During the gap the old Worker still grants media from `phones`, which is the
  old behaviour, with no owners exposed.

---

## Dispositions (the lane, at 48e53c7d)

The reviewer's scenario file is committed beside this one as `self-review-r1-scenarios.test.ts.txt`
(each test asserts the behaviour at 735e5373, so a green run reproduced the finding).

| Finding | Disposition | Where |
|---|---|---|
| MAJOR 1 (S1a, S1b, S2) | **Fixed by a restructure.** These findings and MINORs 1 and 2 are one question: which binding held the id when Meta stamped the message. The routing is now one model of spans: `bound_stamp`, `wa_since` and `phone_aliases.since..changed_at`. A binding newer than the message never takes it, and a previous holding routes to its binding or to nobody, never on to a current holder. | gateway.ts `heldAt`, `pastHolding`, `routeKey`, `stranger`; tests "a message whose first delivery failed, retried after the owner moved..." and "the same for another person...", plus the binding test's today's-shape retry |
| MINOR 1 (S3) | **Fixed.** A change notice strips nobody: its ids are what the owner leaves. The notice's own old number is recorded as a previous holding. Both strips spare a holder newer than the stamp. | test "a notice whose first delivery failed, landing after the old number's next holder joined..." |
| MINOR 2 (S4) | **Fixed.** Every statement of the change block reads the owner that held the notice's ids at its stamp (`ownerKey(..., stamp)`), so a redelivered notice names nobody. | test "a notice Meta redelivers after the owner changed number again..." |
| MINOR 3 (S6) | **Fixed in README rule 7.** The box takes the current id from `GET /v1/window`. The 409 test pins both facts. | bsuid.test.ts, the 409 test |
| MINOR 4 (M4, M1, M16, M2) | **Pinned or gone.** S7 pins the window MAX, and the gate-order test redelivers the notice and checks the window. S8's bind wamids are in reverse order. M4's ORDER BY is gone (8dab7afb): with spans carrying their since it decided nothing, and two bindings' spans holding one stamp now name nobody. M16's guard is gone: the strip's stamp test replaced it. | tests "one number two owners left in turn...", "a retry landing after the owner already wrote...", "when the records disagree...", the gate-order test |
| NIT 1 (no stamp) | **Documented.** README rule 5: "Meta stamps every message"; one without a stamp is read as stamped on arrival. | README |
| NIT 2 (clock clamp) | **Fixed.** Spans compare Meta's raw stamps (`stamp`); the window keeps the clamped one (`at`). | gateway.ts planWebhook |
| NIT 3 (index order) | **Fixed.** 0003 builds `deliveries_wamid_once` before dropping `deliveries_once`. | migrations/0003_retries.sql |
| NIT 4 (evidence path) | **Fixed** by this evidence commit (`deploy-gap.txt`). | here |
| NIT 5 (adapter, cap) | **The adapter:** the contract-change comment on gp-9hf1 and gp-nn90 says the box must resend a 409 to the current id from `GET /v1/window`; the adapter's own round owns the code. **The cap:** documented in README rule 5 as a bounded overshoot. | bead comments; README |

New rules the restructure added, each with its own test and mutation:
- Two spans holding one stamp (records that disagree) route to nobody.
- A number filled later, or taken in a change, is held from that message.
- A known sender's message that no span holds is never answered and is counted
  (`unrouted_messages`).

Cross-check: the reviewer's scenario file run against 48e53c7d's code: S1a, S1b, S2, S3 and S4 no
longer reproduce. S5 (no stamp, documented) and S6 (the newest delivery's old id, documented)
still describe the code, and S7 and S8 assert behaviour the code keeps.

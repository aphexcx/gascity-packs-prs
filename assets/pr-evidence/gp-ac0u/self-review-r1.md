# gp-ac0u self-review, round 1

- **Reviewer.** A fresh general-purpose agent on the lane's own model (Opus 5.5), read-only.
- **What it reviewed.** Commit a2d65a16, against its parent 392f9c09 and the adapter at
  origin/feat/gp-9hf1-whatsapp-pack-adapter ba915531. It read the code only through `git show`,
  because the lane's mutation run was editing `src/` in place, and it did not run the suite.
- **The brief.** The code, the bead's requirements, Meta's facts as measured, and seven focus areas:
  - SQL parameters and NULL semantics;
  - poison batches;
  - routing and identity;
  - migration 0002 on a live D1, and the old Worker on the new schema;
  - privacy;
  - the contract and the adapter;
  - test gaps.
- **The verdict.** No blocker. 1 MAJOR, 7 MINOR, 6 test gaps, 4 NIT.
- **The fixes.** All landed at e71714ab.

| # | Finding | Disposition |
|---|---|---|
| MAJOR 1 | A delivery took `wa_id` from the message alone. Take an owner the gateway knows by phone (row conv=P, wa=P, user=U) whom Meta names by user_id alone (a username, no contact for 30 days). Their message reached the inbox with `wa_id: null`. The adapter at ba915531 (`inbound.js:246`) leaves such a delivery unacknowledged until the 7-day hold drops it. | **Fixed.** A message delivery carries `COALESCE(message id, owner row id)` for both wa_id and user_id. Tests: "an owner bound with both ids ...", and "an owner bound by wa_id alone ..." (which also covers today's shape carrying the known user_id). |
| MINOR 2 | An old number stays reserved as a conversation_id after a number change. (a) A newcomer on the recycled number could never bind. (b) The fill and the re-key could give the newcomer a wa_id that is another row's conversation_id. | **(a) Fixed.** A bind is keyed by its wa_id unless that number is another owner's conversation_id, else by its user_id. The joined row, the code-used mark and the welcome find the bound row by its binding wamid. **(b) Left, as a note for the adapter.** A box keyed by conversation_id never confuses the two. A box keyed by wa_id (ba915531) could, but only when the old owner and the newcomer share one city. That is in the gp-9hf1 notes (PR body section 5, item 1). |
| MINOR 3 | When Meta names a phone beside a different user_id, the stale row kept the phone, so its city's sends went to the new holder. The same held for a re-key whose new number another row holds. | **Fixed.** In the message's group, a row holding the phone beside another user_id loses it (NULL never hits a UNIQUE). The re-key does the same for the new number, unless the holder has no user_id. The owner's sends then go to its user_id. Tests: the recycled-number case (the owner loses the number, is sent to by user_id, and the newcomer joins another city keyed by user_id), and "a number change never makes a payload fail". |
| MINOR 4 | A late notice. A message under the new ids that arrives before a failed notice's retry is treated as a stranger's, and is lost. A code texted in that window binds a second row. | **Documented** as a known limit (README rule 5). The fix would hold strangers' messages for replay, which is a larger change than this bead. |
| MINOR 5 | README Deploy step 3 overstated: (i) a mixed payload answers 500 as a whole; (ii) rows the old Worker writes have no conversation_id; (iii) the old ack leaves the backfilled conversation_id on body-less rows. | **(i)** Documented. **(ii) Fixed:** the inbox reads `COALESCE(conversation_id, wa_id)`. **(iii) Fixed:** the cron clears ids on body-less rows (`stray_ids_cleared`). Tests cover the v1-row fallback and the cron. `deploy-gap.txt` runs the old code on the new schema. |
| MINOR 6 | A status's contacts username was never read, though the bead says to read user_id and username from every status webhook. | **Fixed.** `InboundStatus.username` comes from the contacts entry matched by the recipient's ids. It is delivered, and kept through adoption (`orphan_statuses.username`). Tests: the status parse and the statuses case. |
| MINOR 7 | The new number may be in `system.new_wa_id` (Meta's general webhook reference). | **Fixed:** `wa_id`, else `new_wa_id`. Test added. |
| MINOR 8 | `sends.conversation_id` keeps an id for 30 days and is not listed under retention. | **Documented** (README "Data, retention, logs": Sends, Owners). |
| Gap | Re-key collisions (the poison guards). | **Test added**: "a number change never makes a payload fail ...". |
| Gap | A notice redelivered. | **Test added** (one delivery, one re-key). |
| Gap | A notice and a message in one payload. | **Test added** (the message reaches the owner's conversation, and no canned line goes out). |
| Gap | Media for a user_id-only owner. | **Test added**. |
| Gap | The old code against the new schema. | **Evidence**: `deploy-gap.txt` (`deploy-gap.check.ts`). |
| Gap | Tests that lock in the MAJOR 1 and MINOR 3 behaviour. | **Re-cut** to the fixed behaviour. |
| NIT | `meta.ts`'s doc said a notice with no previous user id is skipped. | **Fixed** (the doc now says what is kept). |
| NIT | A user-less payload from a recycled number reaches the old owner. | **Left.** Meta has sent user_id on every messages webhook since April 2026. |
| NIT | Pre-migration `unknown_senders` rows keyed by phone are orphaned. | **Left.** The cost is one more canned line and a fresh code-attempt day for a stranger within 7 days of the deploy. Noted in the PR body. |
| NIT | `phones.username` is never cleared, and no route reads it. | **Left.** The bead asks for it. The README now says it is kept until unbind. |

The reviewer found these correct: every numbered parameter in all 15 statements; the NULL semantics of `ownerMatch`, `ownerFirst` and `ownerKey`; the SQLite upsert parsing; no reachable constraint error from a signed payload; `conversation_id` never updated; the redelivery idempotency of a notice; migration 0002 under D1's foreign keys; privacy (logs, admin listing, ack, cron, healthz); the additive contract; and the stranger keying.

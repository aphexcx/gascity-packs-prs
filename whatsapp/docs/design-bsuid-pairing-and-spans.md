# WhatsApp gateway: the pairing and span model

For Afik's approval before PR 76's build round (gp-ijky). Lines cite
`whatsapp/gateway/src/gateway.ts` at `e39c0b90` unless marked `main` (`8efd7eb1`).

## 1. The problem

PR 76 routes a message by rebuilding each number's holders from binds, number-change notices, and
messages naming a number beside a user id. Meta retries failed writes for days, in any order, so a
fact often lands after the message that needed it. Five rounds each found another such order:

| Round | Evidence order | Wrong route |
|---|---|---|
| 1, gate r1 (`:707` at `1e0a2483`) | A's message under its old ids fails; A's notice commits; the retry | Dropped as a stranger's |
| 2, gate r2 (`:823` at `ba82ab46`) | C's number-only image from P fails; C unbound; A shows P; B takes P; A's notice; the retry | A's city |
| 3, gate r3 (`:839`) | C holds P, no user id; A pairs P; A's number-only image fails; A's notice skips P; the retry | C's city |
| 4, gp-byp8 codex r5 (`:410` at `17f86937`) | A's pairing of P fails; C takes id V; C's number-only image fails; A's notice; the pairing lands; C unbound; the retry | A's city |
| 4b, gp-sv2d review (R4, R9) | A newcomer's code text beside C's P fails; the city re-mints; the retry | C's city: C adopts the newcomer |

From round 2 on, every misroute the reviews found needed a message naming no user id or a binding
holding none.

## 2. The model

**Route by the user id, never the number.** Since early April 2026 the sender's user id "will be
assigned to the `user_id` parameter and appear in all messages webhooks" ([Meta,
BSUIDs](https://developers.facebook.com/documentation/business-messaging/whatsapp/business-scoped-user-ids)).
It "uniquely identifies a WhatsApp user"; a number change replaces it, by a notice naming both.

1. A message goes to the binding answering to its user id at its stamp, or to no city, counted. A
   notice names its `previous_user_id`. One naming none reaches no city: its contradicting pairing
   may still be in flight (T7).
2. A binding answers to its bound id and each id notices link to it, from the bind (or that id's
   last unbind, if later) until the notice that replaces it. Notices become links in any order:
   reached ones re-key the binding, others wait 8 days, replays change nothing. One id, one binding:
   a code binds only an id no binding answered to in the last 8 days, and a link to an id another
   binding holds links nothing (as `:869` does). An unbind records, for 8 days, every id its binding
   answered to in that time. A notice without `previous_user_id` links nothing; the owner is
   re-invited.
3. **Every send goes to a user id**, the welcome and canned line included (`meta.ts:287-:288` puts
   it in Meta's `recipient`). A number can change hands unseen; a user id cannot.
4. **"Late" is a per-number high-water mark that only picks which binding shows a number**: the one
   whose current id holds its newest pairing (for the admin view, or a `to` naming it). Any message
   pairing the number with a user id moves the mark unless stamped below it; a notice moves its new
   number and clears its old. This replaces rule 2's strip (`:664-:672`), the fill (`:796-:799`) and
   the re-key's number (`:865-:867`).
5. **No adoption.** A user id no binding answers to is a stranger's: its live code binds; any other
   text, a typo included, reaches no city. Today a row without a user id takes any message beside
   its number (`:277`, `:328`) and keeps the id (`:800-:801`).
6. **Codes stay as PR 73 built them.** `invites()` deletes the city's unspent code when it mints a
   new one (`main` `:680`); round 4b proposed keeping it stamped revoked and testing liveness at the
   text's stamp (`:706`). Only adoption needed that.
7. **A message arriving before the evidence its route needs is counted, never held or refused.**
   Under a new user id whose notice has not landed, it is a stranger's, as today (gateway README
   `:257-:260` at `e39c0b90`). A refusal makes Meta redeliver every stranger's first text for days;
   holding stores strangers' texts.

## 3. Shape (B) compared

(B) records every observed (number, user id, stamp) and derives a number's holder per message, exact
and immune to order. It costs an interval algebra in SQL per message, and still misroutes a
number-only message that beats its contradicting pairing. The model keeps (B) where exact (point 2's
links) and (A)'s mark for display, and this note recommends it over both.

## 4. Acceptance tests

Each runs as Meta sends, then number-only, where nothing may reach a city or bind. C holds P in W,
with and without a user id; A is in X by a, B in Y by b.

| Test | Order | Expected |
|---|---|---|
| T1, 4b finding 1 (R1, R2, R2b, R6, R7) | P paired with a and b in every order, first writes failing | a's to X, b's to Y, media likewise; X's sends reach a, never P's holder |
| T2, finding 2 (R3, R3b, R3c) | A on Q, Q re-paired; C's image from P fails; P paired with a; C unbound; the retry | Never X |
| T3, finding 3 (R4, R4b, R8) | A newcomer's code text beside P fails; the code dies (re-minted, spent, or the newcomer binds); the retry | No city; C unchanged; the live code binds to X |
| T4, finding 4 (R9) | The newcomer: a six-digit typo, then X's live code | The typo: no city; the code binds to X; C unchanged |
| T5, gate r3 (round 4 scope 1) | P paired with a (T2); A's image (T3) fails; A's notice (T4); the retry, C standing; also on time | X, with media; never W |
| T6, new | a→a2 (T1) fails until a2→a3 (T2) lands; then a→a2 is replayed | A answers to a3. `e39c0b90` leaves A on a2, hearing the canned line (`:833`, `:868`) |
| T7, new | P's pairing with a (T2) fails; A's image (T3) arrives on time; T2 lands | X. Number-only, `e39c0b90` and round 4b's head both give W, with media |
| T8, new | A's code text for Y fails, before or after A binds to X; A's message to X fails; X unbinds A, or a notice replaces a; both retried | Never Y; X while A stands. `e39c0b90` gives it to Y after the unbind, against README `:572` |

Each enters RED where `e39c0b90` fails it (round 4's regressions R1, R2, R2b, R3 and R3c route
correctly there). The PR body carries the reproductions.

## 5. Migration and compatibility

PR 76's draft keeps 0002's keys, one delivery row per wamid with its media (0003 `:13-:14`,
`:764-:771`), attempts counted by wamid (0003 `:49-:62`), previous user ids routed until their
change (`:838`), `bound_stamp`, the 409 (`:1077-:1082`) and the 8-day stale filter (`:580`). It
drops the number spans (`phone_aliases`' number rows, `wa_since`, the guard `:839`) and adoption,
and rewrites README rules 2, 3, 5 and 7 and its unbind (`:560-:575`), a raw `DELETE` today. Main has
only 0001, so 0003 changes in place: drop `wa_since`; add the mark (number, user id, stamp), the
links (old id, new id, stamp) and unbinds (user id, time).

PR 73's code serves Meta's test number, Live mode unpublished (`docs/meta-setup.md:23-26`, step 8).
PR 73 binds by number alone (`main` `:543`; `0001_init.sql:45-46`), so its owners have no user id;
the Fable reads of 11/5 found none, and the mayor recounts before the deploy (Q1). Meta's page gives
the retry window, "for up to 7 days" ([Meta,
webhooks](https://developers.facebook.com/docs/whatsapp/cloud-api/guides/set-up-webhooks)), so PR
73's 500s reach the new code within 7 days, with user ids.

## 6. Open questions for Afik

1. **Owners PR 73 bound, without a user id.** (a) The mayor unbinds and re-invites each at the
   deploy (recommended; none on 11/5). (b) Each adopts its next message's user id, reopening T4.
2. **A message under a new user id before its notice.** (a) A stranger's, as today (recommended).
   (b) Held 8 days for the notice.
3. **A code whose city minted a newer one.** (a) Dead at once, as PR 73 does (recommended). (b)
   Stamped revoked, so a text sent while it was live still binds (point 6).

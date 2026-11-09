# gp-lv49 fresh adversarial review r1 (one Opus subagent, on 67a5bb31)

The reviewer got the design note, the delta e39c0b90..67a5bb31 and the builder's seven decisions,
but not the builder's reasoning. It worked in its own detached worktree
(`/private/tmp/gp-lv49-review/tree`, left in place; scratch tests `test/review-*.test.ts`; raw run
`/private/tmp/gp-lv49-review/scratch-run.txt`). The harness kept it from writing this file, so the
lane transcribed its final report here.

**Counts: 0 CRITICAL, 2 MAJOR, 5 MINOR, 4 NIT.** Routing by user id holds: in none of the delivery
orders tried does a message, its media or a send reach another person's city, unless Meta reissues
a retired user id. Both MAJORs come from one gap in point 2 ("notices become links in any order").

## MAJOR, and what the round did

- **F1. A bind made after its id's notice landed keeps the retired id.** A's code text under a
  fails, A's notice a->a2 lands (its link waits), the code text's retry binds X under a. X never
  re-keys: A's messages under a2 are a stranger's, and the welcome and X's sends go to a. **Fixed
  in 0507b4c6** (the lane's own read found it first): the re-key also runs after a bind, and the
  welcome reads the binding's current id after the commit. RED at 26dc6ce2:
  `rekey-after-bind-red-at-26dc6ce2.txt`.
- **F2. An unbind that lands before the owner's notice does not cover the new id.** A in X by a;
  the a->a2 notice fails; under a2 A sends Y's code and a message for X, both failing; X unbinds A
  (records a); the notice lands; the retries bind Y and give it the message. **Fixed in cac1792c**
  (the codex self-review r1 found the same, its finding 1): a late link extends every unbind record
  of its old id at or after the notice's stamp along the chain. RED at c17245a8:
  `../codex-r1/red-at-c17245a8.txt`. The reviewer's control F2c (the same texts landing on time,
  before the notice) sends them to Y, as point 7 says it must.

## MINOR

- **F3.** A `user_changed_user_id` notice with no `from` and no `previous_user_id` is dropped by
  `meta.ts:246` and never counted in `notices_without_previous_id` or `unrouted_messages`.
- **F4.** A payload carrying [the owner's message under a2, then the a->a2 notice] plans the message
  first: it reaches no city and hears the canned line though the evidence came in the same request.
- **F5.** A notice refused because Y holds a2 (A became a stranger there, point 7), then Y unbinds
  A, then Meta replays the notice: the replay links and re-keys X to a2.
- **F6.** The re-key ignores stamps; it matters only if Meta reissues a retired user id.
- **M1.** A stranger's message (including point 7's owner writing under an id whose notice has not
  landed) is never counted in `unrouted_messages`.

## NIT

- **N1.** `unrouted_messages` counts arrivals, not wamids.
- **N2.** A code text stamped in the same second as its id's unbind binds nothing (strict `>` over
  Meta's stamp and the gateway's clock).
- **N3.** Unbinding by a number an owner shows, or by an old user id, unbinds nothing.
- **N4.** Chains past 16 links read as a stranger's (fixed in cac1792c, the codex finding 3); a
  number-only notice still clears its old number's mark (display only); the 5,000-held cap's
  pre-read misses owners reached through a link (a bounded overshoot, as before).

## Checked and sound (the reviewer's list)

Number-only messages never route or bind; PR 73 rows never route and a send to one is 403; every
send, the welcome and the canned line go to `recipient`; recursive CTEs terminate on NULL anchors
and cycles, with no poison batch; each `walk` CTE is scoped to its subquery; every bound `?N` is
referenced; no UNIQUE conflict can throw a batch; the 400-statement split falls between message
groups; a welcome goes only for a committed bind; the cron's 8-day deletes line up with the stale
filter; the 409 behaves; logs carry no ids, numbers, bodies, codes or tokens; 0003 matches section
5. Decision 1 (an unbound owner can rebind at once) is consistent with T8; the note's literal
"8 days" reads stricter, worth a word from Afik.

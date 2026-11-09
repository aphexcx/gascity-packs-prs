# gp-w376 self-review r1: a fresh reviewer on ba82ab46..eaf93d2b

- **The reviewer.** One fresh general-purpose subagent (Opus), given the delta, the two gate records it answers (the codex gate r2 and the Fable read r1) and six probes. It was not given the lane's reasoning. It read committed content only, and ran its scenarios in its own detached worktree (`/private/tmp/gp-w376-review/tree` at eaf93d2b, scratch tests in `test/scratch-review.test.ts` there), never in the lane's tree.
- **Its verdict.** 1 CRITICAL, 0 MAJOR, 2 MINOR, 2 NIT. Full suite at eaf93d2b: 146/146.
- **The codex self-review on the same delta read CLEAN** (`codex-self-review-r1-eaf93d2b.txt`). The CRITICAL below is why the round moved past eaf93d2b.

## The findings and what the lane did

1. **CRITICAL: gateway.ts:835 (`UNION ALL SELECT ?3, w ...`) together with the fill (gateway.ts:795-801).** The notice's old number still started at the bind when another row held it every time a message could have given it to the owner. Reproduced at eaf93d2b:
   - **T0:** A joins X by user_id a alone. C joins W phone-only on P.
   - **T0 + 1 h:** C's image from P; its first delivery fails.
   - **T0 + 2 h:** A's text shows P beside a. Rule 2 skips C (no user_id), and the fill is blocked by C, so A's `wa_since` stays NULL.
   - **T0 + 4 h:** A's notice from P writes `phone_aliases (P, since T0, changed_at T0 + 4 h)`.
   - **T0 + 4.5 h:** W's operator unbinds C.
   - **T0 + 5 h:** C's image is retried. It lands in X, and `GET /v1/media` on X answers 200.

   The same happens without the T0 + 2 h message.
   - **Disposition: FIXED in 1d85f5ba.** The bead's rule allows bound_stamp only when the number was held from the binding's start. A's row never held P, and `COALESCE(wa_since, bound_stamp)` could not tell that row from one bound with P and then stripped. Both rule-2 strips now keep the start of the number they take. The notice's old number starts at the raw `wa_since`, and is recorded only when there is one. RED first: two tests (`vitest-red-self-review.txt`).
   - **The reviewer's fix sketch, not taken.** It suggested writing `wa_since` on a blocked fill and marking a disputed alias. With the fix above, a blocked message gives the owner no previous holding at all. So while C stands, P's message stamped T1 is C's, as its on-time delivery already was, and after C's unbind it is nobody's.
2. **MINOR: README.md:208 vs the fill on a change notice.** When the old number is free at the notice, the notice's own fill gives it a zero-length holding (since = changed_at). An owner known by user_id alone then gets no holding of a number the records never showed before its notice. Its own phone-only retry from before the change is a stranger's (the canned line at that number). The same at the base.
   - **Disposition: NAMED.** That is this round's rule: the records never gave the owner the number. README rule 5 gains "A second known limit".
3. **MINOR: `unrouted_messages` and the new `unrouted` log field count deliveries, not wamids.** A message Meta delivers twice counts twice, and a payload that answered 500 writes no log line.
   - **Disposition: NAMED.** README says both count deliveries. A per-wamid marker would be new schema, beyond the Fable read's "fold where cheap".
4. **NIT: `owner_user_id` may be a user_id Meta already replaced while a notice is on its way.** `GET /v1/window` shows the same id.
   - **Disposition: README rule 7 says so.**
5. **NIT: the 409 test cannot tell a wrong row pick** (the owner `to` keys is always the first row read).
   - **Disposition: FIXED in 1d85f5ba.** A new test writes the pair with the other owner's row first. The mutant "the 409 names the first row D1 reads" is killed only by it.

## The probes, as the reviewer answered them

1. **Span starts.** Bind, fill, a change with a free, a held or no shared number, the strip and the unbind agree, except the blocked fill (finding 1) and the notice's own fill (finding 2). No regression against the existing tests.
2. **wa_since readers.** heldAt and both strips read it only beside a non-NULL wa_id. Every path that sets wa_id sets wa_since. Only the alias's start changed, as intended.
3. **The 0003 backfill.** `attempts_since + 86400` is never earlier than the count. The insert is idempotent. The gap counts nothing, because the v1 stranger path answers 500 on 0002's rename.
4. **The 409.** It is city-scoped, at most one row has `conversation_id = to`, it never names another city's owner, the number or the other owner. Only finding 4 remains.
5. **The log count.** It counts committed statements only, with finding 3's limits.
6. **The tests.** The span table catches the bind-start and the old `wa_since = NULL` mutants. It had no row with a blocked message (finding 1). The backfill test fails without the backfill. The 409 test had finding 5.

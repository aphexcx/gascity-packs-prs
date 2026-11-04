# Codex QUICK self-review, round 3 (gp-28td): the round-3 delta of PR 75

- Command: `/Users/tailor512/city/.gc/shims/cxswap/codex exec --sandbox read-only -`
  with `codex-round3-prompt.md` on stdin, from the lane worktree (the hub
  launcher). The prompt named the diff
  `git diff f881606fd1dfefce3936a6260bd1d5be361e2eb1..HEAD` (HEAD
  `ec1a5d7d`: 09f49556, ec1a5d7d) and six questions: ids, phone numbers or
  credentials left; the page without its ids; step 13's order; step 10's
  clause; the new quotes; the new verbs.py check and red.py mutation.
- Header: OpenAI Codex v0.153.3, model gpt-6-astra, provider hub, sandbox
  read-only, reasoning effort xhigh, session
  01a1a8f7-e0de-7972-8564-21070d58e39b.
- Started 2026-11-04T23:49:38Z, ended 23:52:16Z, rc 0, 61,967 tokens.
- The raw transcript is not committed: codex read the old head, so it
  echoes the ids this round takes off (10 lines). It stays in the lane's
  scratch directory.
- The Meta spot-check was skipped: the read-only sandbox could not resolve
  Meta's hosts. Codex checked the new quotes against the round-3 table in
  `meta-pages-read.md`; the lane's own fetch at 23:41Z to 23:42Z matched
  every quote.

## Codex's verdict (verbatim, but for its two example numbers)

Codex wrote its two examples with zeros; they are written here with `N`
for the last seven digits, since the folded check fails any 555 number in
this directory.

> No P1 findings. Two scanner gaps:
>
> - **P2 — verbs.py:152:** The “no 555 phone number” check misses E.164 (`+1555NNNNNNN`) and parentheses (`+1 (555) NNN-NNNN`). **Fix:** handle country codes and parentheses, and add both as mutation cases.
> - **P3 — verbs.py:151:** Grouped IDs and IDs touching letters escape detection; the exemption is broader than hexadecimal commit IDs. Ordinary URL paths, query parameters, and code spans are caught. **Fix:** normalize grouped ID candidates and exempt complete hashes explicitly.
>
> Otherwise:
>
> - No remaining Meta IDs, phone numbers, or credentials found in the tracked HEAD files in scope. The two allowed commit-ID matches are harmless.
> - No new account contradictions, incorrect step references, or status mismatches found. The named objects remain identifiable.
> - Step 13’s subscription/readback and both-language **APPROVED + UTILITY** gate are sound alongside the existing registration, Live-mode, and billing steps. “Stays as step 7 set it” includes step 7’s custom-domain callback move.
> - Step 10 accurately distinguishes the founders’ DBA plan from Meta’s legal-name relationship requirement.
> - No current scanner false positives. Potential ones include public Help Center article IDs, numeric-only hash excerpts, and epoch timestamps in evidence.
> - `verbs.py` passes. Reproducing the fourth mutation in memory produces exactly one failure with exit 1: it proves bare-ID detection, not phone-format coverage.
>
> Network DNS resolution failed, so I skipped the live Meta spot-check. The new quotes match the recorded evidence tables, including round 2’s Graph-reference quote.

## The fold

Both findings are about the guard this round added; neither touches the
page. Both were reproduced, then fixed in verbs.py, with a mutation for
each form in red.py.

- P2: the 555 pattern now takes an optional `+1` or `1` with a separator
  and optional parentheses around 555, so `+1555NNNNNNN` and
  `+1 (555) NNN-NNNN` fail like `+1 555 NNN NNNN`.
- P3: whole hex hashes (hex tokens with at least one letter: commit ids,
  sha256 prefixes) are taken out first, by name, instead of exempting any
  run that touches a letter. Then any run of 13 or more digits fails, also
  when grouped by single spaces, dashes or dots or when touching letters.
  The compact UTC stamps in the city's record file names
  (`YYYYMMDD-HHMMSS`, 14 digits around a dash) are taken out too, as dates
  (the bead: "a date or a Unix time that matches is renamed or allowed with
  a reason"); the widened pattern flagged two of them in
  `meta-pages-read.md` before that rule.
- red.py: four new mutations, each form in a file of its own (`+1555…`,
  `+1 (555) …`, a 16-digit id in groups of four, an id touching letters),
  all built at run time so red.py holds no digit run. Exactly eight FAIL,
  RED OK (`runs/red.r3-fold.out`). The pre-fold verbs.py (`ec1a5d7d`) on
  the same mutated mirror fails only the old four and misses all four new
  forms (`runs/verbs.r3-prefold-misses.out`).
- After the fold: verbs.py at the head passes every check
  (`runs/verbs.r3-fold.out`); against `git archive f881606f` it still fails
  on exactly `meta-setup.md` (8) and `meta-pages-read.md` (7)
  (`runs/verbs.r3-fold-red.out`).

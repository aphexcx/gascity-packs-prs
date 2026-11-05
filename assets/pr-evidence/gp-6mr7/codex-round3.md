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
page. Both were reproduced, then patched in verbs.py at `8c31287a`, with a
mutation for each form in red.py. The r2 below found that patch still
open, and the check changed shape (the last section).

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

## Self-review r2, on the fold `8c31287a` alone

- Same launcher and model, prompt `codex-round3-r2-prompt.md` (the diff
  `git diff ec1a5d7d..8c31287a`), session
  01a1a8fc-e1a4-7bf1-896e-2605884ec879, 23:55:06Z to 23:57:14Z, rc 0,
  35,063 tokens.

Codex's verdict, verbatim but for one example, whose 16-digit run is written
`1NNNNNNNNNNNNNN1` here (the bead's acceptance grep reads this directory):

> No P1 findings. The four new examples are caught, but the scanner still has gaps:
>
> - **P3 — verbs.py:156:** The hash exemption swallows `a1NNNNNNNNNNNNNN1` and `+15550000000a`. Hex-looking tokens need not be hashes, so P3 remains open. **Fix:** exempt known hashes or verified hash contexts and add these mutations.
> - **P3 — verbs.py:157:** The timestamp exemption hides invalid `20999999-999999` and removes the timestamp-shaped portion of grouped ID `1-20261104-235959`, letting both pass. **Fix:** restrict exemptions to validated timestamps in recognized record filenames, excluding larger numeric candidates.
> - **P2 — verbs.py:159:** The original E.164/parentheses examples are fixed, but `+1 555 000 0000` still passes (` ` means nonbreaking space). Double-space-grouped IDs also escape line 158. **Fix:** normalize horizontal whitespace within numeric candidates and add mutations.
>
> The requested worktree command exited **0 with only PASS lines**; no current false positives.
>
> No findings in **red.py**: in-memory execution confirmed each new form independently produces one failure; combined mutations produce eight. Its actual predicate rejects seven, nine, or a successful scanner exit.
>
> **codex-round3.md** clearly discloses the intentional `N` substitutions and correctly describes the tested mutations. However, “Both … fixed” at line 47 overstates closure given these bypasses.

## The change of shape (after r2)

Two rounds on the same check each found forms around the last patch. A
third pattern would invite a third round, so the check no longer guesses
at the shape of an id. It looks for the values citadel's records hold
(the operand a writer cannot move): every run of 12 or more digits, and
the last ten digits of every +1 phone number, in `meta-ids.env` and the
mayor's 11/4 session record (nine values: the seven ids, the test number,
one more long run of that record). A file fails if its digits, every other
character deleted, contain one of them. Separators of any kind, letters
around a value, a hex-looking token or a phone format leave the digits as
they are, so the hash and stamp exemptions are gone, and with them both of
r2's P3s. The check fails closed: an unreadable record, or fewer than eight
values, is a FAIL (`runs/verbs.r3-known-closed.out`). Its stated boundary:
an id the records do not hold yet is the bead's acceptance grep's to catch.

red.py is now the table that specifies it: thirteen hostile rows, each a
recorded value in one form named by r1 or r2 (grouped by spaces, by
non-breaking spaces, by double spaces, by dashes; touching letters; behind
a hex letter; in a URL; split like a stamp; the number in E.164, E.164
touching a letter, with parentheses, with non-breaking spaces, national
form), plus four controls that must pass (a commit id, a UTC stamp, epoch
milliseconds, an unrecorded 16-digit number). The recorded values are read
at run time and written only into the scratch mirror. Exactly seventeen
FAIL (three skill mutations, the page, thirteen rows), no control among
them, RED OK (`runs/red.r3-known.out`). At the head every check passes
(`runs/verbs.r3-known.out`); against `git archive f881606f` the check fails
on exactly the page (8) and the facts map (7) (`runs/verbs.r3-known-red.out`).

## Self-review r3, on the change of shape `fcbb7a60` alone

- Same launcher and model, prompt `codex-round3-r3-prompt.md` (the diff
  `git diff 8c31287a..fcbb7a60`), session
  01a1a902-6974-7380-b6cd-758a2608829e, 00:01:08Z to 00:04:48Z (11/5),
  rc 0, 43,760 tokens. Codex was asked not to copy a recorded value into
  its answer, and did not.

Codex's verdict (verbatim):

> No P1 findings. Three findings:
>
> - **P2 — verbs.py:177:** Numeric markup can interrupt the digit stream while displaying the complete recorded ID. `[FIRST_HALF](https://example.invalid/v2)SECOND_HALF` passed with actual recorded halves in memory. The URL’s `2` breaks the match. **Fix:** scan rendered Markdown/HTML text alongside the raw digit stream.
>
> - **P2 — verbs.py:173:** Phone extraction still accepts only narrow separators. Reformatting the phone with NBSPs in both records drops it; eight other values satisfy the count, and a literal phone leak passes. **Fix:** normalize record phone whitespace and validate that the required phone was extracted independently of the aggregate count.
>
> - **P3 — red.py:56:** The “stamp-shaped” row splits sixteen digits **8–8**, which never matches r2’s **8–6** timestamp exemption. Its invalid-stamp and digit-prefixed-stamp cases remain untested. **Fix:** use synthetic record fixtures covering both exact r2 forms.
>
> Other checks:
>
> - The requested command exited **0 with only PASS lines**. Current records supply all seven IDs and the phone; no current extraction omission found. Nonphone values shorter than twelve digits are ignored.
> - An empty env record passes because the session supplies all nine values. An empty session, unreadable record, or total below eight fails.
> - In memory, all thirteen hostile rows failed individually, all four controls passed, and combined mutations produced exactly seventeen failures. The actual RED predicate rejects sixteen, eighteen, exit 0, or a control replacing a hostile failure.
> - Joining unrelated numbers can produce false positives, but none occurred; ordinary growth from the current roughly 4,600 scanned digits seems unlikely to cause one.
> - `codex-round3.md` faithfully preserves the historical verdicts and disclosed substitutions. Its timestamp-coverage claim inherits the table issue above.
>
> Spelled-out digits and encodings such as base64 were excluded. No files were changed.

The fold keeps the shape (one function: the recorded values in a file's
digits) and adds what r3 found to the table:

- Markup: the check reads two digit streams per file, the raw text and the
  text with Markdown link targets, reference labels and HTML tags taken
  out, and fails a value found in either. Three rows: a value split by a
  link with a digit in its target, by a reference label, by an HTML tag.
- The phone: a record's `+1` number is read across any spacing (the
  non-breaking space included), and each kind has its own proof: at least
  seven ids, and at least one number, each a FAIL line when missing. The
  aggregate count is gone.
- The stamp rows: the 8-8 row is named for what it is (`red-id-split-8-8`),
  and r2's two exact forms (`20999999-999999`, `1-20261104-235959`) and an
  NBSP-spaced number are rows against a synthetic record that red.py
  writes beside the real ones, outside the scanned tree. The known-values
  shape had no stamp exemption left, so those two already failed; the rows
  pin it.
- RED for the fold: the new table against `fcbb7a60`'s verbs.py misses the
  three markup rows and the NBSP-spaced number, RED NOT SEEN
  (`runs/red.r3-known-prefold.out`). With the fold: exactly twenty-three
  FAIL (three skill mutations, the page, nineteen rows), no control among
  them, RED OK (`runs/red.r3-known-r3fold.out`). Against `git archive
  f881606f`: the page (8) and the facts map (7) only
  (`runs/verbs.r3-known-r3fold-red.out`). An unreadable record as the only
  record: three FAIL, the record and each kind
  (`runs/verbs.r3-known-r3fold-closed.out`).
- Left as stated limits: spelled-out digits and encodings (base64, HTML
  numeric entities); and an id not yet in the records, for the bead's
  grep.

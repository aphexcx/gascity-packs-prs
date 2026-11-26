# Round 2: the review lanes read the fix step's earlier answers

This round's delta runs from `4617acb5398596bdba8157a6fcec55292e107e60`,
round 1's head, to this file's commit. It has six commits, each a test
commit followed by a text commit:

| Commit | What it does |
| --- | --- |
| `8a7815db` | The new test, red against the base's step text |
| `6cd07bd1` | The step text |
| `6397be8c` | The self-review's rows, red |
| `94d02196` | The self-review's text folds |
| `1fd5b75f` | The Codex round's rows, red |
| `da80ad96` | The Codex round's text folds |

## How the checks ran

Each command ran as one plain line under `env -i`. The environment was
the one the verify skill at main builds:

- PATH with Python 3.12.13 (pytest 9.2.0) and Node 24.21.0 first;
- HOME and TMPDIR in scratch;
- `GC_NO_INSTALL=1`, `GC_TOOLCHAIN_NODE_INSTALL=off`,
  `GIT_TERMINAL_PROMPT=0`, `GIT_ALLOW_PROTOCOL=file`,
  `GOTOOLCHAIN=local`, `GOPROXY=off` and `GOSUMDB=off`;
- the Go module and build caches.

Each run used a clean tree: the commit was extracted with `git archive`
into a scratch directory and made a one-commit repository of its own.

## The asset file, red then green

The test is `test_review_lanes_read_the_fix_steps_earlier_answers` in
`gascity/tests/test_formula_assets.py`, beside round 1's
`test_review_loop_steps_state_the_review_rules`.

- TC reads the loop template's children from
  `gascity/formulas/build-basic-review.formula.toml`.
  - Every child whose text sets a lane verdict (`_verdict=approve|iterate`)
    must name `## Earlier Review Rounds`, ANSWER ACCEPTED and ANSWER
    REJECTED. It asserts that there are three such children.
  - The one child that sets `code_review.verdict=iterate` must carry the
    sentence that records each round's summary in that section, whether
    or not the pass commits.
- T1 to T8 read the texts the formula loads.
- T7 checks that the lanes' read, folded from its first sentence to its
  last, is the same text in all three lanes and names no lane.

The command, from `gascity`:
`python3 -m pytest tests/test_formula_assets.py -q -p no:cacheprovider`

| Revision | Result | Exit |
| --- | --- | --- |
| `4617acb5` (the base) | 140 passed, 7028 subtests passed | 0 |
| `8a7815db` | 62 failed (TC 10, T1 5, T2 9, T3 15, T4 6, T5 4, T6 8, T7 4, T8 1; all in the new test; its city-neutral guards and the lane and fix-step counts pass), 141 passed, 7033 subtests passed | 1 |
| `6cd07bd1` | 141 passed, 7095 subtests passed | 0 |
| `6397be8c` | 12 failed (the changed T1, T2, T3, T6 and T8 rows, and T7, whose read lacked the changed sentences), 141 passed, 7084 subtests passed | 1 |
| `94d02196` | 141 passed, 7096 subtests passed | 0 |
| `1fd5b75f` | 3 failed (the changed T5, T6 and T8 rows), 141 passed, 7094 subtests passed | 1 |
| `da80ad96` | 141 passed, 7097 subtests passed | 0 |

No test that exists at the base failed at any of these revisions.

## Handoff suites

These ran at `da80ad96`, the last commit that changes step text or a
test, in the order shown, one at a time. Round 1's counts are beside
them.

| Command | Round 2 at `da80ad96` | Round 1 | Exit |
| --- | --- | --- | --- |
| `make test-gascity 'PYTEST_ARGS=-q -p no:cacheprovider'` | 1085 passed, 128 skipped, 10804 subtests passed | 1084 passed, 128 skipped, 10735 subtests passed (`4fa5f65e`) | 0 |
| `python3 -m pytest` over the fourteen CI directories, `-q -p no:cacheprovider` | 3124 passed, 132 skipped, 2 warnings, 11033 subtests passed | 3123 passed, 132 skipped, 2 warnings, 10964 subtests passed (`4617acb5`) | 0 |
| `node --test --test-skip-pattern '^review 4: archive replay advances in bounded chunks without dropping rows$'` in `reachable/test` | 218 passed, 1 skipped | 218 passed, 1 skipped (`4617acb5`) | 0 |

The one more test in each Python count is this round's test.

At `6cd07bd1`, an earlier run of `make test-gascity` failed one test,
`test_city_claim_command_bounds_ambiguous_hook_failures_without_drain_ack`.
Its child process hit the test's two-second timeout. This round changes
nothing the claim command reads. The same test passed in the fourteen-directory run
that followed at that commit, and in both runs at `da80ad96`.

## The hand walk

No live loop was run. Each row is walked by hand at `da80ad96`, round by
round, against the table of the round's spec. Rounds are counted at the
formula's default cap, `max_attempts = 3` (formula line 32).

The files are abbreviated as follows. Every path is under
`gascity/assets/workflows/build-basic-review/`.

| Short name | File |
| --- | --- |
| FIX | `{target}.apply-review-findings.md` |
| SYN | `{target}.synthesize-review.md` |
| ACC | `{target}.acceptance-review.md` |
| TE | `{target}.test-evidence-review.md` |
| SIMP | `{target}.simplicity-review.md` |

The three lanes carry the same read paragraph: ACC 28-48, TE 19-39 and
SIMP 18-38. An ACC line below is the same sentence nine lines earlier in
TE and ten lines earlier in SIMP.

Rows 7a to 7c are walked for a finding that is blocking when it is
answered, as R5 of the spec reads them. A residual risk answered DOES NOT
MATTER that later comes back blocking matches no earlier blocking finding
(FIX:8-9, "compare each blocking finding with the blocking findings in all
earlier rounds"). It meets FIX:66-80 in whatever round it comes back.

### Row 1: a required fix, committed in round 1; round 2's lanes approve. Cell: 2, pass

- **Round 1.**
  - SYN classifies the finding a required fix (SYN:4-5).
  - FIX commits the fix and writes "FIXED (with the commit" (FIX:57).
  - FIX records the summary: "Record every review-fix summary this step
    writes ... keep one line per round" (FIX:82-87).
  - FIX sets `iterate`: "`code_review.verdict=iterate` when this pass
    commits a fix" (FIX:193).
- **Round 2.**
  - The lanes read the section (ACC:28-30). The finding's line is "no
    answer: judge the current commit as you would without it" (ACC:43-44).
  - The fix is in the current commit, so all three lanes approve.
  - FIX writes the no-op summary: "If all three review lanes approve,
    write a no-op review summary" (FIX:43-44).
  - FIX sets `done`: "only when this pass commits no fix and acceptance,
    test evidence, and simplicity all approve" (FIX:190-191).
  - **2, pass.**

### Row 2: a required fix answered WRONG in round 1; round 2's lane accepts the answer. Cell: 2, pass

- **Round 1.**
  - FIX writes "WRONG (the reviewer was mistaken, with the line that shows
    it)" (FIX:58-59), with no commit.
  - It records the summary "whether or not the pass commits" (FIX:84).
  - It sets `iterate`: "A required fix settled WRONG, INTENDED or DOES NOT
    MATTER sets `iterate` too" (FIX:195-196).
- **Round 2.**
  - The lane finds the line (ACC:31-32). The line "is an answer: check it
    against the current commit, by the line or reason it gives"
    (ACC:32-35).
  - The answer holds, so the lane does "not report the finding: list it
    once in your report as ANSWER ACCEPTED" (ACC:36-38), and "with no
    finding left but accepted answers, record `approve`" (ACC:47-48).
  - SYN: "An ANSWER ACCEPTED entry is not a finding and takes none of the
    four classes above" (SYN:17-18).
  - FIX: round 2 runs no family comparison ("From round 3 onward",
    FIX:8). The entry "takes no line in the summary" (FIX:79-80).
  - All three lanes approve: no-op summary (FIX:43-44), recorded
    (FIX:82), `done` (FIX:190-191).
  - **2, pass.**

### Row 3: the same, answered INTENDED. Cell: 2, pass

As row 2, with INTENDED in FIX:59. The lane checks the reason the line
gives (ACC:35). **2, pass.**

### Row 4: a required fix answered DOES NOT MATTER. Cell: 2, pass

As row 2, with DOES NOT MATTER in FIX:59. **2, pass.**

### Row 5: missing evidence supplied in round 1 by a proof with no commit; round 2's lane accepts it. Cell: 2, pass

- **Round 1.**
  - TE sets `iterate` because "proof is missing or insufficient"
    (TE:91-93), and SYN classifies the finding missing evidence (SYN:5).
  - FIX runs the proof and writes "FIXED (... for missing evidence the
    proof command and its result)" (FIX:57-58).
  - It records the summary (FIX:82-87).
  - It sets `iterate` "when this pass ... supplies missing evidence, so the
    review lanes see it" (FIX:193-194).
- **Round 2.**
  - In TE, "FIXED by a proof command and its result" is an answer, which
    TE "may run again" (TE:23-27). It holds: ANSWER ACCEPTED (TE:27-29),
    and TE records `approve` (TE:38-39).
  - The other lanes approve. FIX writes a no-op summary and sets `done`
    (FIX:43-44, 190-191).
  - **2, pass.**

### Row 6a: an answer of round 1 REJECTED in round 2; the fix step commits a fix, or supplies another proof, in round 2; round 3's lanes approve. Cell: 3, pass

- **Round 1.** As row 2, or row 5 for a proof.
- **Round 2.**
  - The lane: "When it does not hold, report the finding as ANSWER
    REJECTED with the round of the answer and the reason the answer fails"
    (ACC:38-39).
  - SYN: the finding "keeps the class it has, of the four above, and
    carries the round of the answer and the lane's reason" (SYN:19-21).
  - FIX reads the listed summaries (FIX:66-68). The finding "has been
    rejected" (FIX:70-72).
  - Its line is one of "FIXED with a commit; for missing evidence, FIXED
    with a proof command that no earlier summary records for it"
    (FIX:73-74).
  - "Each of the three sets `iterate`" (FIX:78).
  - FIX records round 2's summary at "a path of its own" (FIX:87-90).
- **Round 3.**
  - FIX:8-9 has no round-3 blocking finding to compare.
  - The latest line counts (ACC:31-32):
    - FIXED with a commit: the lane judges the current commit
      (ACC:43-44) and approves;
    - the new proof: the lane accepts it (ACC:32-38).
  - FIX processes normally: "Otherwise process this pass normally"
    (FIX:19). It commits nothing and all three lanes approve, so `done`
    (FIX:190-191). `done` is not `iterate`, so the exhaustion exit of
    FIX:19-22 does not apply.
  - **3, pass.**

### Row 6b: an answer of round 1 REJECTED in round 2; the fix step neither fixes nor proves. Cell: 3, FAILED at the family stop

- **Round 2.**
  - FIX writes the third line: "a line without an outcome that gives the
    reviewer's reason and says why this pass neither fixes nor proves it"
    (FIX:74-76). FIX:61-64 names this case.
  - `iterate` "when a required fix or missing evidence remains"
    (FIX:194-195).
- **Round 3.**
  - The latest line is round 2's: "A line without an outcome is no answer:
    the finding stands" (ACC:44-45). The lane reports it.
  - FIX:8-16: the finding matches round 1's and round 2's. "On a match,
    apply no fix", and FIX sets `gc.build.repair_status=family_stop` and
    `gc.outcome=fail` (FIX:29).
  - **3, FAILED at the family stop.**

### Row 6c: as 6a, but round 3's lane still reports the finding. Cell: 3, FAILED at the family stop

- Round 3's blocking finding matches the earlier rounds' (FIX:8-16).
- FIX:66 applies only to "a round after the first that the family
  comparison above did not stop".
- **3, FAILED at the family stop.**

### Row 7a: a finding first raised in round 2 and answered there; round 3's lane accepts. Cell: 3, pass

- **Round 1** ends `iterate` on another finding.
- **Round 2.**
  - No earlier summary answered the new finding, so FIX:70-80 does not
    apply. FIX writes WRONG, INTENDED or DOES NOT MATTER under FIX:56-59.
  - It records the summary and sets `iterate` (FIX:195-196).
- **Round 3.**
  - The lane accepts the answer (ACC:32-38) and approves (ACC:47-48), so
    round 3 has no blocking finding to compare (FIX:8-9).
  - `done` (FIX:190-191).
  - **3, pass.**

### Row 7b: a finding first raised in round 2 and answered there; round 3's lane rejects. Cell: 3, FAILED at the family stop

- Round 3's ANSWER REJECTED finding (ACC:38-39) keeps its class
  (SYN:19-21) and matches round 2's blocking finding (FIX:8-16).
- FIX:66 does not act, because the comparison stopped the pass.
- **3, FAILED at the family stop.**

### Row 7c: a finding first raised in round 3 and answered there. Cell: 3, FAILED at the exhaustion exit

- Round 3's finding matches nothing (FIX:8-16).
- FIX:19-22: "If its verdict would be `iterate` on the controller's last
  allowed attempt", and an answer sets `iterate` (FIX:193-196). So FIX
  sets `gc.build.repair_status=exhausted` and the loop ends FAILED.
- The summary is recorded (FIX:82), and no later round reads it.
- **3, FAILED at the exhaustion exit.**

### Row 8: an answer of round 1 accepted in round 2, and another finding fixed with a commit in round 2; round 3's lanes read both summaries. Cell: 3, pass

- **Round 1.**
  - Finding F is answered WRONG and the pass iterates.
  - The summary sits at a path of its own (FIX:87-90), under the line
    `- round 1: <path>` (FIX:87).
- **Round 2.**
  - The lanes accept F (ACC:36-38), and F takes no line in round 2's
    summary (FIX:79-80).
  - Finding G is FIXED with a commit, and the pass iterates (FIX:193).
  - Round 2's summary "never overwrites an earlier round's" (FIX:89-90).
- **Round 3.**
  - The lanes read "every review-fix summary that section lists, whatever
    its round" (ACC:29-30). F's latest line is round 1's answer
    (ACC:31-32), and it holds again, so it is not reported.
  - G's line is FIXED with a commit, so the lane judges the current
    commit (ACC:43-44) and approves.
  - There is no blocking finding (FIX:8-9), so `done` (FIX:190-191).
  - **3, pass.**

### Row 9: a required fix with no implementation worktree; a line without an outcome in every round. Cell: 3, FAILED

- **Round 1.**
  - "If a required fix cannot be tied to an implementation worktree,
    write an iterate summary" (FIX:110-111).
  - The line is the first case of FIX:61-63, a line without an outcome.
- **Round 2.**
  - "A line without an outcome is no answer: the finding stands"
    (ACC:44-45).
  - FIX:70-71 lists the answers, and a line without an outcome is none of
    them, so the line repeats and the pass iterates (FIX:194-195).
- **Round 3.** A family match (FIX:8-16).
- **3, FAILED**, unchanged.

### Row 10: a lane that reported only residual risks or PRECONDITION items. Cell: 1, pass

- **Round 1.**
  - "A residual risk or a PRECONDITION item never forces another loop"
    (FIX:52). Each takes DOES NOT MATTER (FIX:60-61).
  - "a lane that reported only residual risks or PRECONDITION items counts
    as approving" (FIX:191-192), and FIX commits nothing, so `done`
    (FIX:190).
  - The summary is recorded (FIX:82), and no later round reads it.
- **1, pass**, unchanged.

All fourteen rows walk to the cell the table gives.

## The root checkout

The case is covered by FIX:197-201, directly after "the review lanes, not
this lane, retire a required fix" (FIX:196-197):

- An unchanged root checkout "is never a required fix and takes no line in
  the summary".
- A lane that reported it and nothing else, or nothing else but residual
  risks or PRECONDITION items, "counts as approving, whatever verdict it
  recorded". So when it is the only reported issue, the verdict stays
  `done`, which agrees with FIX:99-101.
- Beside other findings, those findings set the verdict by FIX:190-196.

## Transcripts

Each red block is the run's pytest short test summary, copied verbatim. Each green line is the run's final line.

### Red at `8a7815db` (the test alone, the text of the base)

```text
=========================== short test summary info ============================
SUBFAILED(contract='TC', lane='{target}.acceptance-review', mark='## Earlier Review Rounds') tests/test_formula_assets.py::FormulaAssetTests::test_review_lanes_read_the_fix_steps_earlier_answers
SUBFAILED(contract='TC', lane='{target}.acceptance-review', mark='ANSWER ACCEPTED') tests/test_formula_assets.py::FormulaAssetTests::test_review_lanes_read_the_fix_steps_earlier_answers
SUBFAILED(contract='TC', lane='{target}.acceptance-review', mark='ANSWER REJECTED') tests/test_formula_assets.py::FormulaAssetTests::test_review_lanes_read_the_fix_steps_earlier_answers
SUBFAILED(contract='TC', lane='{target}.test-evidence-review', mark='## Earlier Review Rounds') tests/test_formula_assets.py::FormulaAssetTests::test_review_lanes_read_the_fix_steps_earlier_answers
SUBFAILED(contract='TC', lane='{target}.test-evidence-review', mark='ANSWER ACCEPTED') tests/test_formula_assets.py::FormulaAssetTests::test_review_lanes_read_the_fix_steps_earlier_answers
SUBFAILED(contract='TC', lane='{target}.test-evidence-review', mark='ANSWER REJECTED') tests/test_formula_assets.py::FormulaAssetTests::test_review_lanes_read_the_fix_steps_earlier_answers
SUBFAILED(contract='TC', lane='{target}.simplicity-review', mark='## Earlier Review Rounds') tests/test_formula_assets.py::FormulaAssetTests::test_review_lanes_read_the_fix_steps_earlier_answers
SUBFAILED(contract='TC', lane='{target}.simplicity-review', mark='ANSWER ACCEPTED') tests/test_formula_assets.py::FormulaAssetTests::test_review_lanes_read_the_fix_steps_earlier_answers
SUBFAILED(contract='TC', lane='{target}.simplicity-review', mark='ANSWER REJECTED') tests/test_formula_assets.py::FormulaAssetTests::test_review_lanes_read_the_fix_steps_earlier_answers
SUBFAILED(contract='TC', step='{target}.apply-review-findings', mark='writes the list') tests/test_formula_assets.py::FormulaAssetTests::test_review_lanes_read_the_fix_steps_earlier_answers
SUBFAILED(contract='T1', sentence='Record every review-fix summary this step writes, the no-op ') tests/test_formula_assets.py::FormulaAssetTests::test_review_lanes_read_the_fix_steps_earlier_answers
SUBFAILED(contract='T1', sentence="A retry of the same round rewrites that round's line and nev") tests/test_formula_assets.py::FormulaAssetTests::test_review_lanes_read_the_fix_steps_earlier_answers
SUBFAILED(contract='T1', sentence='This write needs no branch and no commit: the context file l') tests/test_formula_assets.py::FormulaAssetTests::test_review_lanes_read_the_fix_steps_earlier_answers
SUBFAILED(contract='T1', sentence="The section is the loop's own, not a source anchor's record.") tests/test_formula_assets.py::FormulaAssetTests::test_review_lanes_read_the_fix_steps_earlier_answers
SUBFAILED(contract='T1', sentence='A pass that ends on a terminal exit without a review-fix sum') tests/test_formula_assets.py::FormulaAssetTests::test_review_lanes_read_the_fix_steps_earlier_answers
SUBFAILED(contract='T4', lane='{target}.acceptance-review', sentence='After reading the review context and before writing any find') tests/test_formula_assets.py::FormulaAssetTests::test_review_lanes_read_the_fix_steps_earlier_answers
SUBFAILED(contract='T4', lane='{target}.acceptance-review', sentence='For each finding you would report, look for its line in thos') tests/test_formula_assets.py::FormulaAssetTests::test_review_lanes_read_the_fix_steps_earlier_answers
SUBFAILED(contract='T2', lane='{target}.acceptance-review', sentence='A line whose outcome names no commit (WRONG, INTENDED, DOES ') tests/test_formula_assets.py::FormulaAssetTests::test_review_lanes_read_the_fix_steps_earlier_answers
SUBFAILED(contract='T2', lane='{target}.acceptance-review', sentence='When the answer holds, do not report the finding: list it on') tests/test_formula_assets.py::FormulaAssetTests::test_review_lanes_read_the_fix_steps_earlier_answers
SUBFAILED(contract='T3', lane='{target}.acceptance-review', sentence='When it does not hold, report the finding as ANSWER REJECTED') tests/test_formula_assets.py::FormulaAssetTests::test_review_lanes_read_the_fix_steps_earlier_answers
SUBFAILED(contract='T3', lane='{target}.acceptance-review', sentence='When you cannot tell whether the answer covers what you see ') tests/test_formula_assets.py::FormulaAssetTests::test_review_lanes_read_the_fix_steps_earlier_answers
SUBFAILED(contract='T3', lane='{target}.acceptance-review', sentence='A line that reads FIXED with a commit is no answer: judge th') tests/test_formula_assets.py::FormulaAssetTests::test_review_lanes_read_the_fix_steps_earlier_answers
SUBFAILED(contract='T3', lane='{target}.acceptance-review', sentence='A line without an outcome is no answer: the finding stands.') tests/test_formula_assets.py::FormulaAssetTests::test_review_lanes_read_the_fix_steps_earlier_answers
SUBFAILED(contract='T3', lane='{target}.acceptance-review', sentence='A listed summary that is missing or cannot be read answers n') tests/test_formula_assets.py::FormulaAssetTests::test_review_lanes_read_the_fix_steps_earlier_answers
SUBFAILED(contract='T2', lane='{target}.acceptance-review', sentence='An accepted answer is no reason for `iterate`: with no findi') tests/test_formula_assets.py::FormulaAssetTests::test_review_lanes_read_the_fix_steps_earlier_answers
SUBFAILED(contract='T4', lane='{target}.test-evidence-review', sentence='After reading the review context and before writing any find') tests/test_formula_assets.py::FormulaAssetTests::test_review_lanes_read_the_fix_steps_earlier_answers
SUBFAILED(contract='T4', lane='{target}.test-evidence-review', sentence='For each finding you would report, look for its line in thos') tests/test_formula_assets.py::FormulaAssetTests::test_review_lanes_read_the_fix_steps_earlier_answers
SUBFAILED(contract='T2', lane='{target}.test-evidence-review', sentence='A line whose outcome names no commit (WRONG, INTENDED, DOES ') tests/test_formula_assets.py::FormulaAssetTests::test_review_lanes_read_the_fix_steps_earlier_answers
SUBFAILED(contract='T2', lane='{target}.test-evidence-review', sentence='When the answer holds, do not report the finding: list it on') tests/test_formula_assets.py::FormulaAssetTests::test_review_lanes_read_the_fix_steps_earlier_answers
SUBFAILED(contract='T3', lane='{target}.test-evidence-review', sentence='When it does not hold, report the finding as ANSWER REJECTED') tests/test_formula_assets.py::FormulaAssetTests::test_review_lanes_read_the_fix_steps_earlier_answers
SUBFAILED(contract='T3', lane='{target}.test-evidence-review', sentence='When you cannot tell whether the answer covers what you see ') tests/test_formula_assets.py::FormulaAssetTests::test_review_lanes_read_the_fix_steps_earlier_answers
SUBFAILED(contract='T3', lane='{target}.test-evidence-review', sentence='A line that reads FIXED with a commit is no answer: judge th') tests/test_formula_assets.py::FormulaAssetTests::test_review_lanes_read_the_fix_steps_earlier_answers
SUBFAILED(contract='T3', lane='{target}.test-evidence-review', sentence='A line without an outcome is no answer: the finding stands.') tests/test_formula_assets.py::FormulaAssetTests::test_review_lanes_read_the_fix_steps_earlier_answers
SUBFAILED(contract='T3', lane='{target}.test-evidence-review', sentence='A listed summary that is missing or cannot be read answers n') tests/test_formula_assets.py::FormulaAssetTests::test_review_lanes_read_the_fix_steps_earlier_answers
SUBFAILED(contract='T2', lane='{target}.test-evidence-review', sentence='An accepted answer is no reason for `iterate`: with no findi') tests/test_formula_assets.py::FormulaAssetTests::test_review_lanes_read_the_fix_steps_earlier_answers
SUBFAILED(contract='T4', lane='{target}.simplicity-review', sentence='After reading the review context and before writing any find') tests/test_formula_assets.py::FormulaAssetTests::test_review_lanes_read_the_fix_steps_earlier_answers
SUBFAILED(contract='T4', lane='{target}.simplicity-review', sentence='For each finding you would report, look for its line in thos') tests/test_formula_assets.py::FormulaAssetTests::test_review_lanes_read_the_fix_steps_earlier_answers
SUBFAILED(contract='T2', lane='{target}.simplicity-review', sentence='A line whose outcome names no commit (WRONG, INTENDED, DOES ') tests/test_formula_assets.py::FormulaAssetTests::test_review_lanes_read_the_fix_steps_earlier_answers
SUBFAILED(contract='T2', lane='{target}.simplicity-review', sentence='When the answer holds, do not report the finding: list it on') tests/test_formula_assets.py::FormulaAssetTests::test_review_lanes_read_the_fix_steps_earlier_answers
SUBFAILED(contract='T3', lane='{target}.simplicity-review', sentence='When it does not hold, report the finding as ANSWER REJECTED') tests/test_formula_assets.py::FormulaAssetTests::test_review_lanes_read_the_fix_steps_earlier_answers
SUBFAILED(contract='T3', lane='{target}.simplicity-review', sentence='When you cannot tell whether the answer covers what you see ') tests/test_formula_assets.py::FormulaAssetTests::test_review_lanes_read_the_fix_steps_earlier_answers
SUBFAILED(contract='T3', lane='{target}.simplicity-review', sentence='A line that reads FIXED with a commit is no answer: judge th') tests/test_formula_assets.py::FormulaAssetTests::test_review_lanes_read_the_fix_steps_earlier_answers
SUBFAILED(contract='T3', lane='{target}.simplicity-review', sentence='A line without an outcome is no answer: the finding stands.') tests/test_formula_assets.py::FormulaAssetTests::test_review_lanes_read_the_fix_steps_earlier_answers
SUBFAILED(contract='T3', lane='{target}.simplicity-review', sentence='A listed summary that is missing or cannot be read answers n') tests/test_formula_assets.py::FormulaAssetTests::test_review_lanes_read_the_fix_steps_earlier_answers
SUBFAILED(contract='T2', lane='{target}.simplicity-review', sentence='An accepted answer is no reason for `iterate`: with no findi') tests/test_formula_assets.py::FormulaAssetTests::test_review_lanes_read_the_fix_steps_earlier_answers
SUBFAILED(contract='T5', sentence='Read no review-fix summary and judge no answer: carry the la') tests/test_formula_assets.py::FormulaAssetTests::test_review_lanes_read_the_fix_steps_earlier_answers
SUBFAILED(contract='T5', sentence='An ANSWER ACCEPTED entry is not a finding and takes none of ') tests/test_formula_assets.py::FormulaAssetTests::test_review_lanes_read_the_fix_steps_earlier_answers
SUBFAILED(contract='T5', sentence='A finding a lane marks ANSWER REJECTED keeps its class, requ') tests/test_formula_assets.py::FormulaAssetTests::test_review_lanes_read_the_fix_steps_earlier_answers
SUBFAILED(contract='T5', sentence='When one lane accepts an answer and another rejects it, it i') tests/test_formula_assets.py::FormulaAssetTests::test_review_lanes_read_the_fix_steps_earlier_answers
SUBFAILED(contract='T6', sentence='In a round after the first that the family comparison above ') tests/test_formula_assets.py::FormulaAssetTests::test_review_lanes_read_the_fix_steps_earlier_answers
SUBFAILED(contract='T6', sentence='A blocking finding of this round that an earlier summary ans') tests/test_formula_assets.py::FormulaAssetTests::test_review_lanes_read_the_fix_steps_earlier_answers
SUBFAILED(contract='T6', sentence='It takes one of three lines and no other: FIXED with a commi') tests/test_formula_assets.py::FormulaAssetTests::test_review_lanes_read_the_fix_steps_earlier_answers
SUBFAILED(contract='T6', sentence='Never write WRONG, INTENDED or DOES NOT MATTER for it again,') tests/test_formula_assets.py::FormulaAssetTests::test_review_lanes_read_the_fix_steps_earlier_answers
SUBFAILED(contract='T6', sentence='Each of the three sets `iterate` by the verdict rules below.') tests/test_formula_assets.py::FormulaAssetTests::test_review_lanes_read_the_fix_steps_earlier_answers
SUBFAILED(contract='T6', sentence='An entry the synthesis carries as ANSWER ACCEPTED is not a f') tests/test_formula_assets.py::FormulaAssetTests::test_review_lanes_read_the_fix_steps_earlier_answers
SUBFAILED(contract='T6', sentence='The only lines without an outcome are a required fix this pa') tests/test_formula_assets.py::FormulaAssetTests::test_review_lanes_read_the_fix_steps_earlier_answers
SUBFAILED(contract='T6', sentence='the single-case line without an outcome is gone') tests/test_formula_assets.py::FormulaAssetTests::test_review_lanes_read_the_fix_steps_earlier_answers
SUBFAILED(contract='T7', lane='{target}.acceptance-review', check='one paragraph') tests/test_formula_assets.py::FormulaAssetTests::test_review_lanes_read_the_fix_steps_earlier_answers
SUBFAILED(contract='T7', lane='{target}.test-evidence-review', check='one paragraph') tests/test_formula_assets.py::FormulaAssetTests::test_review_lanes_read_the_fix_steps_earlier_answers
SUBFAILED(contract='T7', lane='{target}.simplicity-review', check='one paragraph') tests/test_formula_assets.py::FormulaAssetTests::test_review_lanes_read_the_fix_steps_earlier_answers
SUBFAILED(contract='T7', check='the same text in every lane') tests/test_formula_assets.py::FormulaAssetTests::test_review_lanes_read_the_fix_steps_earlier_answers
SUBFAILED(contract='T8') tests/test_formula_assets.py::FormulaAssetTests::test_review_lanes_read_the_fix_steps_earlier_answers
62 failed, 141 passed, 7033 subtests passed in 25.61s
```

### Green at `6cd07bd1`

```text
141 passed, 7095 subtests passed in 27.48s
```

### Red at `6397be8c` (the self-review rows)

```text
=========================== short test summary info ============================
SUBFAILED(contract='T1', sentence="Each round's review-fix summary has a path of its own, keyed") tests/test_formula_assets.py::FormulaAssetTests::test_review_lanes_read_the_fix_steps_earlier_answers
SUBFAILED(contract='T2', lane='{target}.acceptance-review', sentence='When the answer holds, do not report the finding: list it on') tests/test_formula_assets.py::FormulaAssetTests::test_review_lanes_read_the_fix_steps_earlier_answers
SUBFAILED(contract='T3', lane='{target}.acceptance-review', sentence='When you cannot tell whether the answer covers what you see ') tests/test_formula_assets.py::FormulaAssetTests::test_review_lanes_read_the_fix_steps_earlier_answers
SUBFAILED(contract='T2', lane='{target}.test-evidence-review', sentence='When the answer holds, do not report the finding: list it on') tests/test_formula_assets.py::FormulaAssetTests::test_review_lanes_read_the_fix_steps_earlier_answers
SUBFAILED(contract='T3', lane='{target}.test-evidence-review', sentence='When you cannot tell whether the answer covers what you see ') tests/test_formula_assets.py::FormulaAssetTests::test_review_lanes_read_the_fix_steps_earlier_answers
SUBFAILED(contract='T2', lane='{target}.simplicity-review', sentence='When the answer holds, do not report the finding: list it on') tests/test_formula_assets.py::FormulaAssetTests::test_review_lanes_read_the_fix_steps_earlier_answers
SUBFAILED(contract='T3', lane='{target}.simplicity-review', sentence='When you cannot tell whether the answer covers what you see ') tests/test_formula_assets.py::FormulaAssetTests::test_review_lanes_read_the_fix_steps_earlier_answers
SUBFAILED(contract='T6', sentence='Never write WRONG, INTENDED or DOES NOT MATTER for it again,') tests/test_formula_assets.py::FormulaAssetTests::test_review_lanes_read_the_fix_steps_earlier_answers
SUBFAILED(contract='T7', lane='{target}.acceptance-review', check='one paragraph') tests/test_formula_assets.py::FormulaAssetTests::test_review_lanes_read_the_fix_steps_earlier_answers
SUBFAILED(contract='T7', lane='{target}.test-evidence-review', check='one paragraph') tests/test_formula_assets.py::FormulaAssetTests::test_review_lanes_read_the_fix_steps_earlier_answers
SUBFAILED(contract='T7', lane='{target}.simplicity-review', check='one paragraph') tests/test_formula_assets.py::FormulaAssetTests::test_review_lanes_read_the_fix_steps_earlier_answers
SUBFAILED(contract='T8') tests/test_formula_assets.py::FormulaAssetTests::test_review_lanes_read_the_fix_steps_earlier_answers
12 failed, 141 passed, 7084 subtests passed in 18.08s
```

### Green at `94d02196`

```text
141 passed, 7096 subtests passed in 16.57s
```

### Red at `1fd5b75f` (the Codex round's rows)

```text
=========================== short test summary info ============================
SUBFAILED(contract='T5', sentence='A finding a lane marks ANSWER REJECTED keeps the class it ha') tests/test_formula_assets.py::FormulaAssetTests::test_review_lanes_read_the_fix_steps_earlier_answers
SUBFAILED(contract='T6', sentence='A listed summary that is missing or cannot be read answers n') tests/test_formula_assets.py::FormulaAssetTests::test_review_lanes_read_the_fix_steps_earlier_answers
SUBFAILED(contract='T8') tests/test_formula_assets.py::FormulaAssetTests::test_review_lanes_read_the_fix_steps_earlier_answers
3 failed, 141 passed, 7094 subtests passed in 18.15s
```

### Green at `da80ad96`

```text
141 passed, 7097 subtests passed in 16.84s
```

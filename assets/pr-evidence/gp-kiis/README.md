# gp-kiis evidence: THE REVIEW RULES

The third note in `gascity/template-fragments/gc-standing-notes.template.md`
(`## Review rules`, after THE PR BODY LINES), its README sentence, and its
tests. Base: main at `7876f976`. Round 1 ended at `0e6e35df` (code head
`d845e279`; the two commits after it touch only this directory). Round 2, the
mayor's gate round 1 on `0e6e35df`, ends at code head `b994cbf3`; its section
is [the last one below](#round-2-the-mayors-gate-round-1), and the files it
names carry its commits in their names. Files named for a round 1 commit
describe that commit, not the head.

Round 1:

| Commit | What |
| --- | --- |
| `0102dcec` | the tests, red first (round 1's facts test) |
| `866934f8` | the note and the README sentence |
| `4b0af8fe` | the self-review folds |
| `d845e279` | the codex round 1 folds; the facts test pins the whole note |

`<gc>` below is a dev build of gc (`gc version` prints `dev`). The `verify`
function is the one in `.claude/skills/verify/SKILL.md` at the base, run with
Python 3.12 (pytest, PyYAML, jsonschema) and Node 24 first on PATH; it clears
the worker's own environment, `GC_TEMPLATE` and `GC_TEST_BIN` included.
Every suite ran in a clean `git clone` of the lane at the named commit, never
in the lane worktree.

## Commands and counts

| Run | Tree | Result |
| --- | --- | --- |
| `red-round1-0102dcec.txt`: `verify python3 -m pytest tests/test_gc_standing_notes_fragment.py -q -p no:cacheprovider -rf` | `0102dcec` (the tests on main's fragment and README) | 4 failed, 11 passed, exit 1 |
| the same | `866934f8` | 15 passed, exit 0 |
| `red.txt`: the same, `--tb=line` | `d845e279`'s tests on main's fragment and README | 7 failed, 11 passed, exit 1 |
| `green.txt`: the same | `d845e279` | 18 passed, exit 0 |
| `red-codex-folds.txt`: the same tests, `--tb=line` | `d845e279`'s tests on `4b0af8fe`'s fragment (before the codex folds) | 2 failed, 16 passed, exit 1 |
| `GC_TEST_BIN=<gc> python3 -m pytest tests/test_gc_role_prompt_integration.py -q -p no:cacheprovider -k "standing_notes or pr_body_lines or code_ownership or new_fragment or without_the_calls"` | `866934f8`, `4b0af8fe` | 11 passed, exit 0 (each) |
| `verify python3 -m pytest tests contributing-to-gascity/tests gascity/tests discord/tests github/tests oversight-rig/tests slack-full/tests slack-channel/tests pr-pipeline/tests profiler/tests whatsapp/tests gchat/tests scripts/tests follow-through/tests -q -p no:cacheprovider` | `866934f8` | 3101 passed, 132 skipped, exit 0 (608 s) |
| `verify make test-gascity 'PYTEST_ARGS=-q -p no:cacheprovider'` | `866934f8` | 1064 passed, 128 skipped, exit 0 (394 s) |
| the full Python suite above | `d845e279` | 3104 passed, 132 skipped, exit 0 (635 s) |
| `make test-gascity` above | `d845e279` | first run, the result of record: 1 failed, 1066 passed, 128 skipped, exit 2 (392 s) at a load of 10 to 13; the one failure is `gascity/tests/test_toolchain_shims.py::PnpmShimGateRoundTests::test_a_signal_during_the_release_is_lost_and_nothing_is_left_standing`, a timing test this diff does not touch, which passed 3 of 3 alone here and 3 of 3 alone at the base at the same load (`suites-d845e279.txt`) |
| `make test-gascity` above, a second run | `d845e279` | 1067 passed, 128 skipped, exit 0 (388 s) |
| CI's gc step: `GC_TEST_BIN=<gc> python3 -m pytest tests/test_gc_role_prompt_integration.py tests/test_pack_composition_warnings.py -q -p no:cacheprovider` | `d845e279` | 103 passed, exit 0 |

Each red is a new test failing for the missing note or README sentence, or
for the text before a fold (the `E` lines in each file).
`tests/test_gc_role_prompt_integration.py` is unchanged: it builds its
expected render from the fragment, so the new note needs no edit there.

## Files

- `suites-d845e279.txt`: the closing lines of each suite at `d845e279`, the
  one `make test-gascity` failure as pytest printed it, and that test run
  alone at the head and at the base with the load beside each run.
- `rendered-notes.txt`: a real gc render (`gc prime --strict`) at `d845e279`
  of a city lane whose prompt calls `gc-role-worker` and then
  `gc-standing-notes`, made by `render_notes.py` with the integration test's
  own helpers. Each note appears once, in order, the notes are byte for byte
  the fragment's body, no slash, date or bead id renders, and the only digits
  in THE REVIEW RULES are the rule numbers. The stderr bytes are the fixture
  city's two standing warnings (it imports neither the core pack nor the
  beads pack; the prompt budget line), the same with and without the call
  line.
- `mutations-prefold-866934f8.txt`: the self-review's four surviving
  mutations, all green on the tests before the folds.
- `mutations-4b0af8fe.txt`: 24 mutations of the text at `4b0af8fe`, all red.
- `mutations-d845e279.txt`: 32 mutations of the text at `d845e279` (the list
  is `mutations-d845e279.json`), all red, including codex's added sentence
  (m25), which the per-subject counts at `4b0af8fe` let through.
- `mutate.py`: the mutation runner. Each mutation writes one text change into
  a scratch clone, runs the fragment tests, and writes the original text back.
- `codex-r1-4b0af8fe.md`: the lane's codex round 1 (STANDARD) at `4b0af8fe`,
  its header and its final review. The worktree path is replaced by
  `<lane worktree>`. Since round 2, the header's session identifier is
  `<removed>` and the two source links point at the files at main `7876f976`
  (identical at `4b0af8fe`); the review text is otherwise as codex wrote it.

## Review rounds and outcomes

Self-review (a fresh reviewer subagent on the lane's model, at `866934f8`):
BLOCKER 0, MAJOR 2, MINOR 5, NIT 4.

| Finding | Outcome |
| --- | --- |
| MAJOR 1: "shown" meant a second model's answer in rule 3 and a reproduction in rule 2, so past round three a second model's "real" could buy a round | FIXED `4b0af8fe`: "a second model's answer alone is not enough" |
| MAJOR 2: "one of four outcomes" left no word for findings the rules list (stopped, NOT SHOWN, PRECONDITION) | FIXED `4b0af8fe`: "or else the listing these rules give it"; the class grew at `d845e279` (codex P2, OPEN) |
| MINOR 3: the family stop had no object; a reproduced repeated family could still be fixed past round three | FIXED `4b0af8fe`: "shown or not, stops the PR: fix nothing more in that round", "hand the PR to the mayor as stopped, not done" |
| MINOR 4: rule 5's "asks for more than these rules" read both ways | FIXED `4b0af8fe`: "where it asks more of you than any rule here (every finding fixed, say), the class wins" |
| MINOR 5: rule 5's depth sentence was not pinned (two mutations green) | FIXED `4b0af8fe` (pinned); `mutations-prefold-866934f8.txt` m01 and m02 |
| MINOR 6: a contradicting sentence added to the note passed (two mutations green) | FIXED `4b0af8fe` (subject counts), then `d845e279` (the whole note pinned) after codex P3 showed a sentence the counts missed |
| MINOR 7 (uncertain): "review round" was never defined | FIXED `4b0af8fe` (a round is one head sent to review), scoped at `d845e279` (codex P2) |
| NIT 8: the README test pinned only the note's name | FIXED `4b0af8fe`: also "stopping a repeated family at round three" |
| NIT 9: the README sentence's "its bead … its city's" | FIXED `4b0af8fe` |
| NIT 10: "a right your city does not run", "cannot show either way", rule 6's word order, "brings" | FIXED `4b0af8fe` |
| NIT 11: rule 6 named the path list differently from THE PR BODY LINES | FIXED `4b0af8fe`: "your city's written list of paths by risk tier" |

Codex round 1 (STANDARD, `4b0af8fe`): P1 1, P2 3, P3 1. Each claim was
checked against the file it cites before the fold.

| Finding | Class, cases, test | Outcome |
| --- | --- | --- |
| P1: a worker in a city on one platform would list a regression on another platform the pack serves as PRECONDITION | A reach clause judged by the worker's own city, not where the changed code runs. Cases: rule 2's "shown reachable in what your city runs", rule 4's "a right your city does not have". Test: `test_review_rules_judge_reach_where_the_changed_code_runs` (red at `4b0af8fe`'s text, `red-codex-folds.txt`; m28, m29) | FIXED `d845e279`; an unknown deployment is not a PRECONDITION |
| P2: rules 5 and 6 apply before findings exist, but the note applied only once a review returned findings | The trigger narrower than the rules; one case. Test: the whole-note pin (m31) | FIXED `d845e279` |
| P2: under a family stop, a new reproduced blocker of the same round had no truthful word | A finding the rules produce with no word for the handoff (self-review MAJOR 2's class). Cases: the listings (`4b0af8fe`), the round's other findings under a stop (`d845e279`, OPEN). Test: `test_review_rules_give_every_finding_a_word_for_the_handoff` (m26, m27) | FIXED `d845e279` |
| P2: "a round is one head" could make two reviews of one head share a round number, which `worker_provenance.py` `summary` reports as conflicting (shown at `gascity/assets/scripts/worker_provenance.py:264-267`) | One case. Test: the whole-note pin (m32) | FIXED `d845e279`: the round count is for these rules only, and the role text's review records give each review its own number |
| P3: an added sentence naming no counted subject passed the counts | Text added outside the pinned clauses went unchecked. Test: `test_review_rules_note_is_exactly_the_six_rules_of_its_bead` (m25) | FIXED `d845e279` |

The codex round 1 folds were not reviewed by a second codex round: the
lane's one STANDARD round is spent, and the gate that follows reads the head.

## Round 2: the mayor's gate round 1

The mayor's gate at `0e6e35df` (DEEP) returned BLOCK: MAJOR 3, MINOR 5,
NOTE 1. Its record stays in the city's own records. The bead names this round
as the mayor's round 1, so rule 2's stop, which starts at round three, did not
apply to it. The round's order was: the lane's own reviewer on the delta, then
one codex round on the delta, no more.

| Commit | What |
| --- | --- |
| `58f17564` | the tests, red first: the whole-note pin starts at the heading and carries the three rule changes; two class tests |
| `7c667507` | the note: rule 2's decision rules on the family alone; past round three, a finding shown any other way is OPEN, NOT SHOWN only for what nobody showed; rule 4's deferred minor is OPEN with its follow-up's reference |
| `7f8c1be4` | the evidence: the session identifier removed, two links re-pointed, the runner and the render helper fixed |
| `2778cf63`, `f7a35e8d` | the first self-review's folds, tests then note |
| `b166108d` | the runner exits 1 unless every mutation is killed |
| `058841e1`, `f73492a0` | codex round 2's folds to the note, tests then note |
| `e00ccfa5` | codex round 2's folds to the runner |
| `22948310`, `53b0ff44` | the second self-review's folds, tests then note |
| `b994cbf3` | the second self-review's folds to the runner (code head) |

### Commands and counts

Every run below used the `verify` function in a clean clone at the named
commit. The before-and-after checks ran each tool as it was and as it is on
one input, in a scratch clone that was left clean.

| Run | Tree | Result |
| --- | --- | --- |
| `red-gate-r1-58f17564.txt`: `verify python3 -m pytest tests/test_gc_standing_notes_fragment.py -q -p no:cacheprovider -rf --tb=line` | `58f17564` (the round's tests on `0e6e35df`'s note) | 3 failed, 17 passed, exit 1 |
| `red-self-review-2778cf63.txt`: the same | `2778cf63` (on `7c667507`'s note) | 2 failed, 18 passed, exit 1 |
| `red-codex-r2-058841e1.txt`: the same | `058841e1` (on `f7a35e8d`'s note) | 1 failed, 19 passed, exit 1 |
| `red-self-review-2-22948310.txt`: the same | `22948310` (on `f73492a0`'s note) | 2 failed, 19 passed, exit 1 |
| `green-b994cbf3.txt`: the same without `--tb=line` | `b994cbf3` | 21 passed, exit 0 |
| `mutations-b994cbf3.txt`: `verify python3 -I mutate.py <clone> mutations-b994cbf3.json <out>` | `b994cbf3` | 55 mutations, 55 killed, exit 0 |
| `mutations-boundary-0e6e35df.txt`: the same with m33 and m42 only | `0e6e35df` (its tests and text) | 2 survived, exit 1: a sentence between the heading and the marker (the gate's NOTE 9) and a heading after the last rule both passed the old tests; both are killed at the head |
| `rendered-notes-b994cbf3.txt`: `python3 render_notes.py <clone> ./build/gc` | `b994cbf3` | `gc prime --strict` exit 0 both ways; each note once, in order, byte for byte the fragment's body; no slash, date or bead id; the review rules' only digits are 1 to 6 |
| `tools-before-after.txt` | the tools at `0e6e35df`, `b166108d` and `e00ccfa5` against the head's | each old tool shows its finding, and the head's does not |
| `suites-14703af1.txt`: `verify python3 -m pytest tests contributing-to-gascity/tests gascity/tests discord/tests github/tests oversight-rig/tests slack-full/tests slack-channel/tests pr-pipeline/tests profiler/tests whatsapp/tests gchat/tests scripts/tests follow-through/tests -q -p no:cacheprovider` | `14703af1` (the head with this round's evidence) | 3107 passed, 132 skipped, exit 0 (599 s) |
| `verify make test-gascity 'PYTEST_ARGS=-q -p no:cacheprovider'` | `14703af1` | 1070 passed, 128 skipped, exit 0 (435 s) |
| CI's gc step: `GC_TEST_BIN=<gc> python3 -m pytest tests/test_gc_role_prompt_integration.py tests/test_pack_composition_warnings.py -q -p no:cacheprovider` | `14703af1` | 103 passed, exit 0 |

`tests/test_gc_role_prompt_integration.py` is unchanged this round too.

### The gate's findings

| Finding | Class, cases, test | Outcome |
| --- | --- | --- |
| MAJOR 1: the round 1 codex transcript's header carried the review session's identifier | An identifier of a private session, conversation, account or machine in pushed evidence. Cases: one in this directory at `0e6e35df` (that header line). The round 2 codex transcript's own header identifier was removed before its commit. Test: the lane's public-repo scan, kept in the city's own records because it names private patterns. Its positive controls now include a transcript header's session line and a bare uuid, and it finds the removed value at `0e6e35df` | FIXED `7f8c1be4`. The value is still in the branch history: `ba594375` added it, and `7f8c1be4`'s own diff shows it removed. Rewriting history is the mayor's call and was not done |
| MAJOR 2: the family stop offered a merge while the round's other blockers sat OPEN | A decision on one finding disposes of others. Cases: the stop's merge offer; an OPEN blocker past round three had no merge gate; a round repeating two families got one decision. Test: `test_review_rules_let_a_decision_on_a_family_dispose_of_that_family_alone` (red at `58f17564`); m34, m35, m43, m46, m47, m50, m51, m52 | FIXED `7c667507`, completed at `f7a35e8d` and `f73492a0` |
| MAJOR 3: a fault an earlier head brought in, proven by a failing test past round three, was listed NOT SHOWN | NOT SHOWN given by the round that brought the fault in, not by the evidence. Cases: an earlier head's fault shown by a failing test, and a finding a second model confirmed, which rule 3 counts as shown. Test: `test_review_rules_list_as_not_shown_only_what_nobody_showed` (red at `58f17564`); m36, m37, m38, m49 | FIXED `7c667507` |
| MINOR 4: a real, unintended minor left for later had no word | One case | FIXED `7c667507`: OPEN with its follow-up's reference; at `f7a35e8d` a minor's follow-up counts as its outcome; m39, m40 |
| MINOR 5: the runner counted a broken test invocation as a kill | | FIXED `7f8c1be4`: a green baseline is required, and a kill needs exit 1 and a named failing test. From `e00ccfa5` both are read from pytest's JUnit XML report |
| MINOR 6: a mutation target could resolve outside the scratch clone | | FIXED `7f8c1be4`: refused when it resolves outside the clone, symlinks included; the original bytes are restored |
| MINOR 7: the render helper lost a caller-relative gc path | | FIXED `7f8c1be4`: resolved before any subprocess runs |
| MINOR 8: two source links in the transcript resolved inside this directory | The same class reached the round 2 transcript's link | FIXED `7f8c1be4`: they point at main `7876f976`, where the cited files are unchanged |
| NOTE 9: the whole-note test started at the marker, not the heading | | FIXED `58f17564`: it runs from `## Review rules` to the end of the define (m33, m41, m42) |

### The first self-review (fresh subagent, the lane's model, at `7f8c1be4`)

BLOCKER 0, MAJOR 2, MINOR 5, NIT 2.

| Finding | Outcome |
| --- | --- |
| MAJOR 1: the stop's OPEN for every other finding gave an untested claim two words (OPEN, and NOT SHOWN from the past-round-three sentence and rule 3) and a merge gate | FIXED for the two words (`f73492a0`, `53b0ff44`). The claim that an OPEN untested blocker breaks rule 3 is WRONG: rule 3 stops a lone reviewer forcing a round or a blind fix, and the mayor's ruling on that one finding before a merge does neither. Codex round 2 P1 held the same |
| MAJOR 2: the session identifier is still in the branch history | OPEN, for the mayor: the lane rewrites no history |
| MINOR 3: "needs its own outcome" said nothing of where the outcome comes from | FIXED `f7a35e8d`: from a later head or the mayor's ruling on that one finding |
| MINOR 4: "each blocking one" dropped part of the order, so a minor beside a stop could merge with no follow-up | FIXED `f7a35e8d`: every OPEN finding needs its own outcome before any merge, and a minor's follow-up counts as one |
| MINOR 5: an OPEN blocker past round three had no merge gate | FIXED `f7a35e8d`: the rule covers every finding listed as OPEN |
| MINOR 6: the two class tests check words, and two mutations passed both | DOES NOT MATTER: the whole-note pin kills both (m49, m50), and the class tests state each class's rule for whoever edits the pin. The IndexError it found is FIXED `2778cf63` |
| MINOR 7: the evidence described the previous head | FIXED: this section and the files named for `b994cbf3` |
| NIT 8: the transcript's changes were not disclosed | FIXED: Files, above |
| NIT 9: the runner exited 0 with survivors; "grade" is undefined; rule 4 never says to file the follow-up | FIXED `b166108d` (exit 1 unless all are killed). DOES NOT MATTER for "grade": rules 1 and 2 say "blocking", and the reviewer's grade carries it. INTENDED for the follow-up: who files it is each city's call, and the note asks for its reference |

### Codex round 2 (the lane's, STANDARD, through the hub, on the delta from `0e6e35df` at `b166108d`)

P1 1, P2 2, P3 2. `codex-r2-b166108d.md` holds its header (session
identifier removed) and its final review.

| Finding | Class, cases, test | Outcome |
| --- | --- | --- |
| P1: beside a stop, a blocker nobody could settle (a deployment the worker cannot check) dropped to NOT SHOWN and out of the merge gate; see `REVIEW.md:44-50`, where a finding nobody can place keeps its grade until the mayor settles it | A blocker beside a stop leaves the merge gate for want of evidence. Cases: the stop's NOT SHOWN (`f7a35e8d`); rule 3's NOT SHOWN beside a stop (the second self-review's MAJOR 1). Test: `test_review_rules_give_each_finding_of_a_stopped_round_one_word` (red at `22948310`); m44, m45, m54, m55 | FIXED `f73492a0`, completed at `53b0ff44`: OPEN, never NOT SHOWN, with what shows each or what would settle it |
| P2: two families repeated in one round got one SAME FAMILY and one decision | One case | FIXED `f73492a0`: each listed, one decision on each repeated family; m46, m47 |
| P2: `PYTEST_ADDOPTS=-q` refused a green clone, and `--color=yes` turned a kill into BROKEN | | FIXED `e00ccfa5`: the JUnit XML report; at `b994cbf3` PYTEST_ADDOPTS is left out |
| P3: a CRLF checkout made every multi-line mutation a setup error | | FIXED `e00ccfa5`: line ends read as newlines; at `b994cbf3` the file keeps its own |
| P3: a red baseline left an earlier report in the out file | | FIXED `e00ccfa5`; at `b994cbf3` the out file is cleared first |

### The second self-review (fresh subagent, the lane's model, on `b166108d..e00ccfa5`)

BLOCKER 0, MAJOR 2, MINOR 4, NIT 2. One class: listing sentences that do not
say which word wins under a stop. Cases: its MAJOR 1, its MAJOR 2 and its
first MINOR. Test: `test_review_rules_give_each_finding_of_a_stopped_round_one_word`.

| Finding | Outcome |
| --- | --- |
| MAJOR 1: rule 3's NOT SHOWN still reached a blocker beside a stop | FIXED `53b0ff44`: "as OPEN, never NOT SHOWN"; m54 |
| MAJOR 2: the stop's OPEN overrode an outcome or a PRECONDITION that needs no fix | FIXED `53b0ff44`: "give every other finding of that round the outcome or PRECONDITION it can have without a fix"; m53 |
| MINOR: what "short of a stop" covered was unclear | FIXED `53b0ff44`: "in a round that does not stop the PR"; m48 |
| MINOR: no class test for one word per finding, or for one decision per family | FIXED `22948310`: the new class test; the family test also pins "one decision on each repeated family" and "its own family alone" |
| MINOR: a crashed run left an earlier report in the out file | FIXED `b994cbf3` |
| MINOR: `PYTEST_ADDOPTS=-k` turned four kills into survivors | FIXED `b994cbf3`: left out of the runs, and a run of fewer tests than the baseline is BROKEN |
| NIT: an empty mutation list exited 0 | FIXED `b994cbf3` |
| NIT: a mutated file lost its CRLF line ends; a test held in a class was skipped | FIXED `b994cbf3` |

No reviewer read `22948310..b994cbf3`, the folds made after the second
self-review. The round's one codex round was spent, and the mayor's gate from
`0e6e35df` reads the head.

### Files (round 2)

- `red-gate-r1-58f17564.txt`, `red-self-review-2778cf63.txt`,
  `red-codex-r2-058841e1.txt`, `red-self-review-2-22948310.txt`: each test
  commit's red on the note before it. `green-b994cbf3.txt`: the head.
- `mutations-b994cbf3.json` and `.txt`: 55 mutations at the head, all killed.
  m01 to m32 come from round 1, re-anchored on the new text; m33 onward are
  this round's, each named for the finding it guards.
- `mutations-boundary-0e6e35df.txt`: m33 and m42 at the gate's head, both
  survived there.
- `tools-before-after.txt`: the runner and the render helper before and after
  each fix.
- `rendered-notes-b994cbf3.txt`: the real gc render at the head.
- `codex-r2-b166108d.md`: the lane's codex round 2, as described above.
- `suites-14703af1.txt`: the closing lines of each suite at `14703af1`. The
  commit after it adds only that file and these rows; at that commit the
  fragment tests and the repository's bare-command scan ran again (the
  closing lines are in the PR body's testing line).

# gp-kiis evidence: THE REVIEW RULES

The third note in `gascity/template-fragments/gc-standing-notes.template.md`
(`## Review rules`, after THE PR BODY LINES), its README sentence, and its
tests. Base: main at `7876f976`. Code head: `d845e279`. The commit after it
adds only this directory.

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
  `<lane worktree>`.

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

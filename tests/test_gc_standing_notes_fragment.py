"""Static checks on the `gc-standing-notes` template fragment (gp-rj21; the
PR body lines, gp-hdn2; the review rules, gp-kiis).

A city opts in with one line after its `gc-role-worker` call; the rendered
order and the byte-identical render without the line are proven against a
real gc in tests/test_gc_role_prompt_integration.py (GC_TEST_BIN). These run
everywhere and pin the note's facts and bounds, which every importing city
renders, so the text must also stay free of any one city's particulars.
"""

from __future__ import annotations

import re
from pathlib import Path

import pytest

REPO_ROOT = Path(__file__).resolve().parents[1]
FRAGMENTS = REPO_ROOT / "gascity" / "template-fragments"
FRAGMENT = FRAGMENTS / "gc-standing-notes.template.md"
ROLE_FRAGMENT = FRAGMENTS / "gc-role-worker.template.md"
README = REPO_ROOT / "gascity" / "README.md"
CALL_LINE = '{{ templateFirst . "gc-standing-notes" }}'
PLAIN_CALL = '{{ template "gc-standing-notes" . }}'
# A template comment without trim markers renders nothing and leaves the text
# around it as it is, so provenance can sit in the source and never reach a
# prompt. A trimmed comment ({{- /* */ -}}) is not matched, so the body check
# below refuses it: it would eat the whitespace next to it.
TEMPLATE_COMMENT = re.compile(r"\{\{/\*.*?\*/\}\}", re.S)
# Each note's heading, in the fragment's order, with its bead of record.
NOTES = (
    ("## Deepening note", "gp-gsww"),
    ("## PR body lines", "gp-hdn2"),
    ("## Review rules", "gp-kiis"),
)
# The PR body lines' labels, in the order the body carries them, and the
# three lines as the note shows them, each label with its own instruction:
# text swapped between two labels fails.
PR_BODY_LABELS = ("Testing:", "Risk:", "How to revert:")
PR_BODY_LINES = (
    "Testing: <the exact commands you ran, each with its counts and exit code>",
    "Risk: <the tier and the path that set it>",
    "How to revert: <the one command that undoes the change once merged>",
)


def fragment() -> str:
    return FRAGMENT.read_text(encoding="utf-8")


def renderable(text: str) -> str:
    """A fragment's text without its template comments: what can reach a prompt."""
    return TEMPLATE_COMMENT.sub("", text)


def flat(text: str) -> str:
    return " ".join(text.split())


def note(marker: str) -> str:
    """One note's renderable text, from its marker to the next heading, on one
    line: a clause hidden in a template comment never reaches a worker, so it
    never counts."""
    text = renderable(fragment())
    assert text.count(marker) == 1
    return flat(text.split(marker, 1)[1].split("\n## ", 1)[0])


def deepening_note() -> str:
    return note("THE DEEPENING NOTE")


def pr_body_lines_note() -> str:
    return note("THE PR BODY LINES")


def review_rules_note() -> str:
    """THE REVIEW RULES whole, from its heading to the end of the define, on
    one line: text between the heading and the marker, or after the last
    rule, reaches a worker as surely as a rule does. The note is the define's
    last; a note added after it lands inside this text until the boundary
    moves with it. Template comments never count."""
    text = renderable(fragment())
    heading = "\n## Review rules"
    assert text.count(heading) == 1
    assert text.count("THE REVIEW RULES") == 1
    body = text.split(heading, 1)[1]
    assert body.count("{{- end }}") == 1
    return flat(heading + body.split("{{- end }}", 1)[0])


def test_fragment_defines_exactly_one_template() -> None:
    text = fragment()
    assert text.startswith('{{ define "gc-standing-notes" -}}\n')
    assert text.endswith("{{- end }}\n")
    assert text.count("{{ define ") == 1
    assert text.count("{{- end }}") == 1
    assert text.count("# Standing Notes") == 1
    # Plain text between the delimiters, comments aside: nothing in the note
    # depends on the render context, so it renders the same in every city and
    # every lane.
    body = renderable(text.split("-}}", 1)[1].rsplit("{{-", 1)[0])
    assert "{{" not in body and "}}" not in body


def test_notes_appear_once_each_in_order() -> None:
    text = fragment()
    for heading, _ in NOTES:
        assert text.count(f"\n{heading}") == 1, heading
    positions = [text.index(f"\n{heading}") for heading, _ in NOTES]
    assert positions == sorted(positions)


@pytest.mark.parametrize(("heading", "bead"), NOTES)
def test_bead_of_record_is_cited_only_inside_a_template_comment(heading: str, bead: str) -> None:
    """The real render without it is proven in the GC_TEST_BIN render test."""
    text = fragment()
    assert f"{heading}{{{{/* Bead of record: {bead}." in text
    assert any(bead in comment for comment in TEMPLATE_COMMENT.findall(text))
    assert bead not in renderable(text)


def test_define_name_is_unique_across_every_pack_fragment() -> None:
    """All imported packs' fragments share one template namespace."""
    defines = [
        (path.relative_to(REPO_ROOT).as_posix(), name)
        for path in sorted(REPO_ROOT.glob("*/template-fragments/*.template.md"))
        for name in re.findall(r'\{\{-?\s*define\s+"([^"]+)"', path.read_text(encoding="utf-8"))
    ]
    owners = [path for path, name in defines if name == "gc-standing-notes"]
    assert owners == ["gascity/template-fragments/gc-standing-notes.template.md"]


def test_deepening_note_carries_every_fact_and_bound_of_its_bead() -> None:
    note = deepening_note()
    for clause in (
        "Once per PR, at your self-review and before the PR is pushed or its head updated",
        "while the whole change is still in your context",
        "on the modules this diff touched, never the whole repository",
        "the codebase-design skill (SKILL.md and DEEPENING.md in your skill directory when installed)",
        "without it, the steps below are the whole method",
        "Name the candidate clusters: a pass-through layer, duplicated logic across callers, "
        "a module whose interface is larger than its body, a seam with one adapter.",
        "Give each its dependency category: in-process, local-substitutable, remote but owned, "
        "or true external.",
        "Apply the deletion test",
        "Decide: deepen now in this PR (only inside the bead's scope, with the tests moved to "
        "the new interface and the old ones deleted), a follow-up bead",
        "name the cluster and its category; the mayor cuts it under the rule of three",
        "or none.",
        "Write the result as a `Deepening note` section at the end of the PR body, three to ten lines",
        "`none` with one reason is a fine answer",
        "Skip the read for docs, config, evidence and diffs under about forty changed lines",
        "say `deepening note skipped: <reason>` in the PR handoff instead (the handoff mail "
        "when the bead names a recipient, else the close reason)",
        "Never run a repository-wide pass from a lane",
        "never add a new seam with a single adapter",
    ):
        assert clause in note, f"THE DEEPENING NOTE lacks: {clause!r}"
    # The steps run in the skill's order: clusters, categories, deletion test, decision.
    steps = ("Name the candidate clusters", "Give each its dependency category",
             "Apply the deletion test", "Decide: deepen now")
    positions = [note.index(step) for step in steps]
    assert positions == sorted(positions)
    assert note.index("Decide: deepen now") < note.index("Write the result as a `Deepening note`")


def test_deepening_note_glosses_the_deletion_test_for_a_lane_without_the_skill() -> None:
    note = deepening_note()
    gloss = note.split("Apply the deletion test", 1)[1].split("Decide:", 1)[0]
    assert "imagine the module deleted" in gloss
    assert "vanishes" in gloss and "pass-through" in gloss
    assert "reappears across its callers" in gloss


def test_pr_body_lines_note_carries_every_fact_of_its_bead() -> None:
    note = pr_body_lines_note()
    for clause in (
        "Whenever you open a PR or update its body",
        "end the body with three lines, each on its own line and starting with its label",
        "after every other section (a Deepening note included)",
        "only the body's closing trailer or sign-off lines may follow them",
        "the exact commands you ran, each with its counts and exit code",
        "write `none: <why>` when you ran none",
        "On the risk line, the tier comes from your city's written list of paths by risk tier: "
        "the highest tier of any path the diff touches, and that path",
        "without such a list, or when no path the diff touches is on it, write `not tiered` "
        "and the highest-risk path the diff touches",
        "the one command that undoes the change once merged",
        # One instruction names every way a PR lands: a revert of the one sha
        # a PR resolves to leaves a rebased PR's earlier commits in place. A
        # range must start at the commit the PR landed on (`A..B` leaves A
        # out; `git revert A B` reverts in the order listed and stops when B
        # touched A's lines), and a fast-forward that merged its base stops
        # at the merge commit without --no-merges.
        "On the revert line, the command runs after the merge, so name what the PR landed as: "
        "the squash commit when squashed; the first-parent change of the merge commit when "
        "merged with two parents; the whole landed range, oldest to newest, when rebased or "
        "fast-forwarded, as one range from the commit it landed on "
        "(`git revert --no-merges <landed-on>..<newest>`, which reverts newest first). "
        "Never name a branch commit the merge may not keep; when you cannot yet tell how the "
        "PR will land, cover each way the repository allows; when no command undoes all of "
        "the change, also say what a revert would leave behind (a migration that ran, "
        "a deploy, a message sent).",
        "On every line, write a secret (a token, a key, a password) as a placeholder such as "
        "`<token>`, never its value",
        "A later round that updates the body rewrites the three lines for its own head "
        "and keeps them after every other section",
        "Keep the labels exactly as written",
    ):
        assert clause in note, f"THE PR BODY LINES lacks: {clause!r}"
    # One revert instruction: a second one could undo the first.
    assert note.count("On the revert line") == 1


# THE REVIEW RULES as the note says them, whole and in order, one part per
# sentence, under the fact of its bead each part carries. The note must be
# exactly these parts: a sentence added anywhere fails, even one that names
# no rule's subject, and so does a clause hidden in a template comment.
REVIEW_RULES = (
    # The heading: text between it and the marker reaches a worker too.
    "## Review rules",
    # When the rules apply (on sending a PR to review as well as on its
    # findings), why, and what a round is for them, apart from the numbers
    # the role text's review records give each review.
    "THE REVIEW RULES.",
    "Whenever you send your PR to review or a review returns findings on it, act by these "
    "rules: each review round costs a full read of the PR, so spend one only where it buys "
    "a fix.",
    "For these rules a round is one head of your PR sent to review, however many reviewers "
    "read it, counted from the round number the bead names, if any; the review records the "
    "role text asks for still give each review its own number.",
    # 1. The class fix and its one test.
    "1. Fix the whole class.",
    "When a review finds a blocking fault, name its class (the general fault, not the one "
    "case), fix every case of that class in your diff, and add one test at the class level; "
    "your handoff names the class, its cases and the test.",
    "A case fixed alone can bring the same finding back on another line in the next round.",
    # 2. A repeated family stops the PR from round three, shown or not, with
    # the round's other findings beside it, OPEN or NOT SHOWN, for the
    # mayor's one decision; that decision rules on the family alone, and
    # every OPEN finding anywhere keeps its own grade and needs its own
    # outcome before any merge, from a later head or the mayor's ruling on
    # it, a minor's follow-up counting; what THE SAME FAMILY is; past round
    # three only a reproduction in the round's own changes buys a round, and
    # a second model's answer is not one; a finding shown any other way is
    # OPEN with its proof and grade, never NOT SHOWN, which is for what
    # nobody showed.
    "2. Stop at round three.",
    "From review round three on, a blocking finding of THE SAME FAMILY as a blocking finding "
    "of an earlier round of this PR, shown or not, stops the PR: fix nothing more in that "
    "round, list it in your handoff as SAME FAMILY with the round and the earlier finding it "
    "repeats, list every other finding of that round beside it as OPEN (as NOT SHOWN when "
    "nobody showed it), and hand the PR to the mayor as stopped, not done, for one decision: "
    "a check for the class built under its own tests, or a merge with the class filed as a "
    "bead.",
    "That decision rules on the repeated family alone: every finding you list as OPEN keeps "
    "its own grade and needs its own outcome before any merge, from a later head or the "
    "mayor's ruling on that one finding, a minor's follow-up counting as its outcome.",
    "THE SAME FAMILY means the finding names the same class as the earlier one, and the "
    "earlier one's class-level fix, written in one sentence, would close it too; when you "
    "cannot tell, it is the same family and you stop.",
    "Past round three, only a finding reproduced in that round's own changes (a failing "
    "test, or shown reachable where the changed code runs; a second model's answer alone is "
    "not enough) buys another round; a finding shown any other way (an earlier head's fault "
    "that a failing test proves, say) buys none by itself and is listed as OPEN with what "
    "shows it and its grade, for the mayor; list the rest, which nobody showed, as NOT SHOWN "
    "with their file and line.",
    # 3. No lone reviewer forces a round: a failing test, or a second model's
    # check of that one finding with the line; else NOT SHOWN, never a blind fix.
    "3. Let no lone reviewer force a round.",
    "Before one reviewer's finding costs a round, have it shown: write a failing test that "
    "shows it, or have a second model check that one finding and answer real or not with the "
    "line that shows it.",
    "A finding shown by neither after an honest try is listed as NOT SHOWN with what you "
    "tried, and is not fixed blind: one model's reading is a suspicion, not a fault.",
    # 4. Minors never block; a real, unintended minor left for later is OPEN
    # with its follow-up; every finding gets one of the four outcomes or the
    # listing a rule gave it; PRECONDITION only for what no place running the
    # changed code has.
    "4. Never let a minor finding block, and give every finding an outcome.",
    "Fix a minor in the same head when it is cheap; a real, unintended minor you leave "
    "unfixed is listed as OPEN with its follow-up's reference.",
    "Your handoff carries one line per finding of every round with its outcome: FIXED (with "
    "the commit), WRONG (with the line that shows the reviewer was mistaken), INTENDED, or "
    "DOES NOT MATTER, or else the listing these rules give it (SAME FAMILY, OPEN, NOT SHOWN "
    "or PRECONDITION).",
    "A finding that needs a runtime, a provider or a right found nowhere the changed code "
    "runs is listed as PRECONDITION with the precondition in one sentence; it does not "
    "block, and you do not build for it.",
    "When you cannot tell where the changed code runs, the finding is not a PRECONDITION.",
    # 5. Depth follows risk; the city sets it by the review class the bead
    # names, which wins wherever it asks more than a rule here.
    "5. Let review depth follow risk.",
    "Docs, tests and small internal changes get your own checks and one gate; a second model "
    "reads a PR's whole first head, and later heads only on a risky path.",
    "Your city or its mayor sets the depth by the review class the bead names: follow it, "
    "and where it asks more of you than any rule here (every finding fixed, say), the class "
    "wins.",
    # 6. The city's written path list, by the name the PR body lines give
    # it: read for the risk line, never invented.
    "6. Take the merge list from its owner.",
    "Your city's written list of paths by risk tier, kept by its founder or owner, says "
    "which paths need a person's word before a merge: read it for the risk line of the PR "
    "body lines above, and never invent one.",
    "Until that list exists, your city's merge rule stands.",
)


def test_review_rules_note_is_exactly_the_six_rules_of_its_bead() -> None:
    assert review_rules_note() == " ".join(REVIEW_RULES)


def test_review_rules_give_every_finding_a_word_for_the_handoff() -> None:
    """Every listing a rule gives a finding (`as SAME FAMILY`, `as OPEN`, ...)
    is one that the outcome rule names beside the four outcomes, and it names
    no other: no finding the rules produce is left without a word."""
    note = review_rules_note()
    given = set(re.findall(r"\bas ((?:[A-Z]+ )*[A-Z]{2,})\b", note))
    named = note.split("or else the listing these rules give it (", 1)[1].split(")", 1)[0]
    assert set(re.split(r", | or ", named)) == given
    # The four outcomes, each once, so one list carries them.
    for outcome in ("FIXED", "WRONG", "INTENDED", "DOES NOT MATTER"):
        assert note.count(outcome) == 1, outcome


def test_review_rules_list_as_not_shown_only_what_nobody_showed() -> None:
    """NOT SHOWN states the evidence, never which round or head brought the
    fault in: each clause that gives it names its findings as ones nobody
    showed, so a finding a failing test proves is never listed NOT SHOWN,
    whichever head of the PR it came from."""
    clauses = re.split(r"(?<=[.;])\s+", review_rules_note())
    giving = [clause for clause in clauses if "as NOT SHOWN" in clause]
    assert giving
    for clause in giving:
        assert re.search(r"\b(?:shown by neither|nobody showed)\b", clause), clause


def test_review_rules_let_a_decision_on_a_family_dispose_of_that_family_alone() -> None:
    """Wherever the rules offer the mayor a merge, the sentence that follows
    keeps every finding listed OPEN at its own grade and owed its own
    outcome before any merge, from a later head or a ruling on that one
    finding: a ruling on one repeated finding never waives another."""
    sentences = re.split(r"(?<=\.)\s+", review_rules_note())
    offers = [index for index, sentence in enumerate(sentences) if re.search(r"\bor a merge\b", sentence)]
    assert offers
    for index in offers:
        assert index + 1 < len(sentences), sentences[index]
        limit = sentences[index + 1]
        for clause in ("alone", "every finding you list as OPEN", "keeps its own grade",
                       "needs its own outcome before any merge",
                       "from a later head or the mayor's ruling on that one finding"):
            assert clause in limit, (sentences[index], clause)


def test_review_rules_judge_reach_where_the_changed_code_runs() -> None:
    """A pack's change runs in every city that imports it: whether a finding
    is reachable, or needs what no deployment has, is judged where the changed
    code runs, never by the worker's own city alone."""
    note = review_rules_note()
    assert re.search(r"\byour city (?:runs|has|does not have)\b", note) is None
    assert note.count("the changed code runs") == 3


def test_review_rules_carry_no_number_but_the_rule_numbers() -> None:
    """A count measured in one city is that city's evidence, not a rule."""
    assert re.findall(r"\d+", review_rules_note()) == ["1", "2", "3", "4", "5", "6"]


def test_pr_body_lines_placement_leaves_room_for_the_closing_trailers() -> None:
    """Every place that says where the three lines go says it one way: after
    every other section, with the body's closing trailer or sign-off lines
    still below them. "Last" would put them under the trailers."""
    note = pr_body_lines_note()
    assert re.search(r"(?i)\blast\b", note) is None, "the note says the lines go last"
    readme = flat(README.read_text(encoding="utf-8").split("## Standing notes", 1)[1].split("\n## ", 1)[0])
    sentence = readme.split("Its second note is THE PR BODY LINES", 1)[1].split(".", 1)[0]
    assert "after every other section, with only trailer or sign-off lines below them" in sentence
    assert re.search(r"(?i)\blast\b", sentence) is None


def test_pr_body_lines_note_shows_each_label_with_its_instruction_once_as_one_block() -> None:
    """The render test proves the same on real gc output; this one runs
    everywhere."""
    text = renderable(fragment()).split("THE PR BODY LINES", 1)[-1]
    for label in PR_BODY_LABELS:
        assert text.count(label) == 1, label
    assert all(line.startswith(f"{label} ") for line, label in zip(PR_BODY_LINES, PR_BODY_LABELS))
    lines = "\n".join(PR_BODY_LINES)
    assert len(re.findall(rf"(?m)^{re.escape(lines)}$", text)) == 1


# One city's particulars: a machine path, a home path, a date, a Slack
# channel or timestamp, a mayor ordinal, the city this pack was built in, a
# bead id of one of that city's stores. A bead id's shape alone (letters, a
# hyphen, a short hash) also matches words like "in-process", so the bead
# case names the prefixes of the stores this pack is built against.
BEAD_ID = r"\b(?:ci|cp|cs|dy|gf|gp|hw)-[a-z0-9]{3,}"
CITY_PARTICULARS = (
    (BEAD_ID, "a bead id"),
    (r"(?<![\w.])/(?:Users|home|private|tmp|opt|var|etc)/", "an absolute machine path"),
    (r"/", "a path (a relative one too)"),
    (r"~/", "a home-relative path"),
    (r"\$(?:GC_CITY_PATH|GC_CITY|HOME)\b", "a city or home variable"),
    (r"\b20\d\d-\d\d-\d\d\b", "a date"),
    (r"\b\d{1,2}/\d{1,2}\b", "a month/day date"),
    (r"\bC0[A-Z0-9]{8,}\b", "a Slack channel id"),
    (r"\b\d{10}\.\d{6}\b", "a Slack message timestamp"),
    (r"\b\d+(?:st|nd|rd|th)\b", "an ordinal"),
    (r"(?i)citadel", "the city name"),
    (r"(?i)\b(?:afik|aphexcx|taylor|sinan)\b", "a founder's or operator's name"),
    (r"(?i)standing note \(", "a mayor-stamped note header"),
)


def test_fragment_text_is_city_neutral() -> None:
    for path in (FRAGMENT, FRAGMENTS / "gc-code-ownership.template.md"):
        text = renderable(path.read_text(encoding="utf-8"))
        for pattern, what in CITY_PARTICULARS:
            match = re.search(pattern, text)
            assert match is None, f"{path.name} names {what}: {match.group(0)!r}"


def test_role_fragment_does_not_call_the_standing_notes() -> None:
    """The call stays city-side: upstream's role text renders unchanged."""
    role = ROLE_FRAGMENT.read_text(encoding="utf-8")
    assert "gc-standing-notes" not in role
    for prompt in sorted((REPO_ROOT / "gascity" / "roles" / "agents").glob("*/prompt.template.md")):
        assert "gc-standing-notes" not in prompt.read_text(encoding="utf-8"), prompt


def test_readme_documents_the_templatefirst_call_line_and_why() -> None:
    readme = README.read_text(encoding="utf-8")
    assert readme.count("## Standing notes") == 1
    section = readme.split("## Standing notes", 1)[1].split("\n## ", 1)[0]
    # The order-proof form is the one call line; the plain form appears once,
    # as the thing to avoid.
    assert section.count(CALL_LINE) == 1
    assert section.count(PLAIN_CALL) == 1
    words = flat(section)
    for clause in (
        "`template-fragments/gc-standing-notes.template.md`",
        'right after its `{{ template "gc-role-worker" . }}` line',
        f"Write it with `templateFirst` (gc v1.2.0 and later), never as `{PLAIN_CALL}`",
        "when the city's import of this pack is older than the commit that carries the fragment",
        "`templateFirst` renders nothing and the lane keeps its role text",
        "the plain call makes gc fall back to the lane's raw prompt source and the lane loses its role text",
        "That fallback prints one stderr line and exits 0, `gc prime --strict` included through gc v1.5.1; "
        "later builds fail `--strict`, but a plain render still returns the raw source",
        "The price of `templateFirst` is silence: an old import or a misspelt name drops the note "
        "without a word",
        "run `gc prime <lane>` and check that the note appears exactly once, after the role text",
        "A city that does not add the line renders exactly what it rendered before",
        "A note that belongs to one city stays in that city's own prompt template",
        "THE DEEPENING NOTE",
        "Its second note is THE PR BODY LINES",
        "`Testing:`, `Risk:` and `How to revert:`, after every other section, "
        "with only trailer or sign-off lines below them",
        "Its third note is THE REVIEW RULES",
        "stopping a repeated family at round three",
    ):
        assert clause in words, f"README Standing notes lacks: {clause!r}"
    assert words.index(CALL_LINE) < words.index(f"never as `{PLAIN_CALL}`")

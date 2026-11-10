"""Static checks on the `gc-standing-notes` template fragment (gp-rj21).

A city opts in with one line after its `gc-role-worker` call; the rendered
order and the byte-identical render without the line are proven against a
real gc in tests/test_gc_role_prompt_integration.py (GC_TEST_BIN). These run
everywhere and pin the note's facts and bounds, which every importing city
renders, so the text must also stay free of any one city's particulars.
"""

from __future__ import annotations

import re
from pathlib import Path

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


def fragment() -> str:
    return FRAGMENT.read_text(encoding="utf-8")


def renderable(text: str) -> str:
    """A fragment's text without its template comments: what can reach a prompt."""
    return TEMPLATE_COMMENT.sub("", text)


def flat(text: str) -> str:
    return " ".join(text.split())


def deepening_note() -> str:
    text = fragment()
    assert text.count("THE DEEPENING NOTE") == 1
    return flat(text.split("THE DEEPENING NOTE", 1)[1])


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


def test_bead_of_record_is_cited_only_inside_a_template_comment() -> None:
    """The real render without it is proven in the GC_TEST_BIN render test."""
    assert any("gp-gsww" in comment for comment in TEMPLATE_COMMENT.findall(fragment()))
    assert "gp-gsww" not in renderable(fragment())


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


# One city's particulars: a machine path, a home path, a date, a Slack
# channel or timestamp, a mayor ordinal, the city this pack was built in, a
# bead id of one of that city's stores. A bead id's shape alone (letters, a
# hyphen, a short hash) also matches words like "in-process", so the bead
# case names the prefixes of the stores this pack is built against.
BEAD_ID = r"\b(?:ci|cp|cs|dy|gf|gp|hw)-[a-z0-9]{3,}"
CITY_PARTICULARS = (
    (BEAD_ID, "a bead id"),
    (r"(?<![\w.])/(?:Users|home|private|tmp|opt|var|etc)/", "an absolute machine path"),
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
    ):
        assert clause in words, f"README Standing notes lacks: {clause!r}"
    assert words.index(CALL_LINE) < words.index(f"never as `{PLAIN_CALL}`")

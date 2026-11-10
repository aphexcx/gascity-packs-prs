"""Static checks on the `gc-code-ownership` template fragment (gp-rj21, amendment 1).

Its whole content is one sentence, verbatim from Anthropic's guide "Prompting
Claude Fable 5.2", section "Say who owns the code":
https://platform.claude.com/docs/en/build-with-claude/prompt-engineering/prompting-claude-fable-5-2#say-who-owns-the-code
It is its own fragment with its own call line, because a city turns it on
lane by lane: the guide says to leave it out where the owner wants the code
changed as little as possible. The render (the sentence once with the call,
byte-identical without it) is proven against a real gc in
tests/test_gc_role_prompt_integration.py (GC_TEST_BIN).
"""

from __future__ import annotations

from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parents[1]
GASCITY = REPO_ROOT / "gascity"
FRAGMENT = GASCITY / "template-fragments" / "gc-code-ownership.template.md"
README = GASCITY / "README.md"
CALL_LINE = '{{ templateFirst . "gc-code-ownership" }}'
PLAIN_CALL = '{{ template "gc-code-ownership" . }}'
GUIDE_URL = (
    "https://platform.claude.com/docs/en/build-with-claude/prompt-engineering/"
    "prompting-claude-fable-5-2#say-who-owns-the-code"
)
# The guide's sentence, byte for byte (straight ASCII apostrophes, as the guide prints it).
SENTENCE = (
    "This codebase is yours: what it's for and how it's built are your call, "
    "and you'll keep building on it."
)


def test_fragment_is_the_guides_sentence_and_nothing_else() -> None:
    assert SENTENCE.isascii()
    assert FRAGMENT.read_text(encoding="utf-8") == (
        '{{ define "gc-code-ownership" -}}\n' + SENTENCE + "\n{{- end }}\n"
    )


def test_no_other_pack_prompt_calls_it() -> None:
    """The call stays city-side, lane by lane: not in the role text, not in
    the standing notes, not in any role prompt this pack ships."""
    surfaces = [
        *sorted((GASCITY / "template-fragments").glob("*.template.md")),
        *sorted((GASCITY / "roles" / "agents").glob("*/prompt.template.md")),
    ]
    assert len(surfaces) > 3
    for path in surfaces:
        if path == FRAGMENT:
            continue
        assert "gc-code-ownership" not in path.read_text(encoding="utf-8"), path


def test_readme_documents_the_call_line_the_measured_effect_and_when_to_leave_it_out() -> None:
    readme = README.read_text(encoding="utf-8")
    assert readme.count("### Code ownership") == 1
    section = readme.split("### Code ownership", 1)[1].split("\n## ", 1)[0]
    words = " ".join(section.split())
    assert section.count(CALL_LINE) == 1
    assert PLAIN_CALL not in section
    assert GUIDE_URL in section
    for clause in (
        "`template-fragments/gc-code-ownership.template.md` defines `gc-code-ownership`",
        "one sentence verbatim from Anthropic's guide",
        "from under a tenth to about half, at about 1.5 times the output tokens",
        "It is its own fragment, not a standing note",
        "a city turns it on lane by lane",
        "in the `templateFirst` form for the reason above",
        "Leave it out where the owner wants the code changed as little as possible",
        "a scope rule elsewhere in the prompt does not cancel it",
    ):
        assert clause in words, f"README Code ownership lacks: {clause!r}"

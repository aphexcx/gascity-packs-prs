"""REVIEW.md at the repository root keeps the shape its readers rely on.

Every reviewer of a pull request here reads REVIEW.md from the base branch.
This test holds the file to four headings and a "Prune by: YYYY-MM-DD" line.
It never compares that date with today, so a calendar date can never turn
the suite red on an unrelated pull request. It reads only the repository's
own file and writes nothing.
"""

from __future__ import annotations

import datetime
from pathlib import Path
import re
import stat
from types import SimpleNamespace


REPO_ROOT = Path(__file__).resolve().parents[1]
REVIEW_MD = REPO_ROOT / "REVIEW.md"

REQUIRED_HEADINGS = (
    "## What MAJOR means here",
    "## What not to report",
    "## Triggers (rules a diff can show)",
    "## Pruning",
)

PRUNE_BY = re.compile(r"^Prune by: (\d{4}-\d{2}-\d{2})$")
HTML_COMMENT = re.compile(r"<!--.*?-->", re.DOTALL)
FENCE = re.compile(r"^ {0,3}(```|~~~)")


def review_md_problems(text: str | None) -> list[str]:
    """Return what is wrong with a REVIEW.md text; None means the file is absent."""
    if text is None:
        return ["REVIEW.md is absent"]
    lines = _rendered_lines(text)
    problems = [
        f"missing heading {heading!r}"
        for heading in REQUIRED_HEADINGS
        if heading not in lines
    ]
    if not any(_prune_date(line) for line in lines):
        problems.append("no 'Prune by: YYYY-MM-DD' line with a real date")
    return problems


def _rendered_lines(text: str) -> list[str]:
    """The lines a reader sees: no HTML comment, no fenced block, no trailing space."""
    lines = []
    open_fence = None
    for line in HTML_COMMENT.sub("", text).splitlines():
        line = line.rstrip()
        fence = FENCE.match(line)
        if fence is not None:
            if open_fence is None:
                open_fence = fence.group(1)
            elif fence.group(1) == open_fence:
                open_fence = None
            continue
        if open_fence is None:
            lines.append(line)
    return lines


def _prune_date(line: str) -> datetime.date | None:
    match = PRUNE_BY.match(line)
    if match is None:
        return None
    try:
        return datetime.date.fromisoformat(match.group(1))
    except ValueError:
        return None


FULL_TEXT = """\
# Review rules

## What MAJOR means here

- MAJOR: a fault.

## What not to report

- What a CI job itself prints.
- Always reported: a secret, a key, a token, an injection or any other
  security fault in the diff.
- Always reviewed by hand: a change to REVIEW.md itself.

## Triggers (rules a diff can show)

- Nothing private.

## Pruning

Prune by: 2027-02-17
"""


def _without_line(text: str, line: str) -> str:
    kept = [kept_line for kept_line in text.splitlines() if kept_line != line]
    assert len(kept) == len(text.splitlines()) - 1, line
    return "\n".join(kept) + "\n"


def test_full_text_has_no_problems() -> None:
    assert review_md_problems(FULL_TEXT) == []


def test_absent_file_is_a_problem() -> None:
    assert review_md_problems(None) == ["REVIEW.md is absent"]


def test_each_missing_heading_is_a_problem() -> None:
    for heading in REQUIRED_HEADINGS:
        problems = review_md_problems(_without_line(FULL_TEXT, heading))
        assert problems == [f"missing heading {heading!r}"], heading


def test_trailing_whitespace_and_crlf_are_not_problems() -> None:
    text = FULL_TEXT.replace("## Pruning", "## Pruning   ").replace(
        "Prune by: 2027-02-17", "Prune by: 2027-02-17\t"
    )
    assert review_md_problems(text.replace("\n", "\r\n")) == []


def test_a_heading_or_date_inside_an_html_comment_does_not_count() -> None:
    text = FULL_TEXT.replace(
        "## Pruning\n\nPrune by: 2027-02-17\n",
        "<!--\n## Pruning\n\nPrune by: 2027-02-17\n-->\n",
    )
    assert review_md_problems(text) == [
        "missing heading '## Pruning'",
        "no 'Prune by: YYYY-MM-DD' line with a real date",
    ]


def test_a_heading_or_date_inside_a_code_fence_does_not_count() -> None:
    for fence in ("```", "~~~"):
        text = FULL_TEXT.replace(
            "## Pruning\n\nPrune by: 2027-02-17\n",
            f"{fence}\n## Pruning\n\nPrune by: 2027-02-17\n{fence}\n",
        )
        assert review_md_problems(text) == [
            "missing heading '## Pruning'",
            "no 'Prune by: YYYY-MM-DD' line with a real date",
        ], fence


def test_a_deeper_heading_does_not_stand_in_for_a_required_one() -> None:
    text = FULL_TEXT.replace("## What not to report", "### What not to report")
    assert review_md_problems(text) == ["missing heading '## What not to report'"]


def test_prune_by_line_needs_a_real_iso_date() -> None:
    bad_lines = (
        "Prune by: 2027-02-30",
        "Prune by: 2027-2-17",
        "Prune by: 20270217",
        "Prune by: soon",
        "Prune by:2027-02-17",
        "Prune by: 2027-02-17 [drafter's judgment]",
    )
    for bad_line in bad_lines:
        text = FULL_TEXT.replace("Prune by: 2027-02-17", bad_line)
        assert review_md_problems(text) == [
            "no 'Prune by: YYYY-MM-DD' line with a real date"
        ], bad_line


def test_missing_prune_by_line_is_a_problem() -> None:
    text = _without_line(FULL_TEXT, "Prune by: 2027-02-17")
    assert review_md_problems(text) == [
        "no 'Prune by: YYYY-MM-DD' line with a real date"
    ]


def test_a_past_prune_date_is_not_a_problem() -> None:
    text = FULL_TEXT.replace("Prune by: 2027-02-17", "Prune by: 2001-01-01")
    assert review_md_problems(text) == []


ALWAYS_REPORTED = (
    "- Always reported: a secret, a key, a token, an injection or any other\n"
    "  security fault in the diff.\n"
)
BY_HAND = "- Always reviewed by hand: a change to REVIEW.md itself.\n"
NO_ALWAYS_REPORTED = (
    "'## What not to report' has no item '- Always reported:' "
    "naming secret, key, token, injection, security"
)
NO_BY_HAND = (
    "'## What not to report' has no item '- Always reviewed by hand:' "
    "naming REVIEW.md"
)
PRUNING_HIDDEN = [
    "missing heading '## Pruning'",
    "no 'Prune by: YYYY-MM-DD' line with a real date",
]


def _replaced(text: str, old: str, new: str) -> str:
    assert text.count(old) == 1, old
    return text.replace(old, new)


def _pruning_wrapped(before: str, after: str) -> str:
    section = "## Pruning\n\nPrune by: 2027-02-17\n"
    return _replaced(FULL_TEXT, section, before + section + after)


def test_each_always_item_is_required() -> None:
    assert review_md_problems(_replaced(FULL_TEXT, ALWAYS_REPORTED, "")) == [
        NO_ALWAYS_REPORTED
    ]
    assert review_md_problems(_replaced(FULL_TEXT, BY_HAND, "")) == [NO_BY_HAND]


def test_an_always_item_names_every_word() -> None:
    for word in ("secret", "key", "token", "injection", "security"):
        item = ALWAYS_REPORTED.replace(f" {word}", " thing")
        assert item != ALWAYS_REPORTED, word
        text = _replaced(FULL_TEXT, ALWAYS_REPORTED, item)
        assert review_md_problems(text) == [NO_ALWAYS_REPORTED], word
    text = _replaced(FULL_TEXT, BY_HAND, "- Always reviewed by hand: this file.\n")
    assert review_md_problems(text) == [NO_BY_HAND]


def test_an_always_item_starts_with_its_marker() -> None:
    lowered = _replaced(FULL_TEXT, "- Always reported:", "- always reported:")
    assert review_md_problems(lowered) == [NO_ALWAYS_REPORTED]
    inside = _replaced(
        FULL_TEXT, "- Always reviewed by hand:", "- Also. Always reviewed by hand:"
    )
    assert review_md_problems(inside) == [NO_BY_HAND]


def test_an_always_item_sits_under_what_not_to_report() -> None:
    text = _replaced(FULL_TEXT, ALWAYS_REPORTED + BY_HAND, "")
    text = _replaced(
        text, "- Nothing private.\n", "- Nothing private.\n" + ALWAYS_REPORTED + BY_HAND
    )
    assert review_md_problems(text) == [NO_ALWAYS_REPORTED, NO_BY_HAND]


def test_an_always_item_split_in_two_does_not_count() -> None:
    split = (
        "- Always reported: a secret, a key, a token.\n"
        "- An injection or any other security fault in the diff.\n"
    )
    assert review_md_problems(_replaced(FULL_TEXT, ALWAYS_REPORTED, split)) == [
        NO_ALWAYS_REPORTED
    ]
    unindented = ALWAYS_REPORTED.replace("\n  security", "\nsecurity")
    assert review_md_problems(_replaced(FULL_TEXT, ALWAYS_REPORTED, unindented)) == [
        NO_ALWAYS_REPORTED
    ]


def test_hidden_always_items_do_not_count() -> None:
    for before, after in (("<!--\n", "-->\n"), ("```\n", "```\n"), ("~~~\n", "~~~\n")):
        text = _replaced(
            FULL_TEXT, ALWAYS_REPORTED + BY_HAND, before + ALWAYS_REPORTED + BY_HAND + after
        )
        assert review_md_problems(text) == [NO_ALWAYS_REPORTED, NO_BY_HAND], before


def test_a_shorter_fence_line_does_not_close_a_longer_fence() -> None:
    text = _pruning_wrapped("````\n```\n", "````\n")
    assert review_md_problems(text) == PRUNING_HIDDEN


def test_a_fence_line_with_text_after_it_does_not_close_the_fence() -> None:
    text = _pruning_wrapped("```\n```text\n", "```\n")
    assert review_md_problems(text) == PRUNING_HIDDEN


def test_an_unclosed_html_comment_hides_the_rest_of_the_file() -> None:
    assert review_md_problems(_pruning_wrapped("<!--\n", "")) == PRUNING_HIDDEN
    assert review_md_problems(_pruning_wrapped("A note <!-- open\n", "")) == (
        PRUNING_HIDDEN
    )


def test_an_unclosed_fence_hides_the_rest_of_the_file() -> None:
    for fence in ("```\n", "~~~\n", "````\n"):
        assert review_md_problems(_pruning_wrapped(fence, "")) == PRUNING_HIDDEN, fence


def test_a_closed_comment_hides_only_itself() -> None:
    text = _pruning_wrapped("<!-- a note -->\n\n", "")
    assert review_md_problems(text) == []
    text = _replaced(FULL_TEXT, "## Pruning\n", "## Pruning <!-- a note -->\n")
    assert review_md_problems(text) == []


class _Disk:
    """A repository root as the checker sees it, without touching a disk."""

    def __init__(self, names: list[str], mode: int, text: str) -> None:
        self.names = names
        self.mode = mode
        self.text = text
        self.reads = 0

    def listdir(self, root: Path) -> list[str]:
        return self.names

    def lstat(self, path: Path) -> SimpleNamespace:
        assert path.name == "REVIEW.md", path
        return SimpleNamespace(st_mode=self.mode)

    def read(self, path: Path) -> str:
        self.reads += 1
        return self.text

    def problems(self) -> list[str]:
        return repository_problems(
            Path("root"), listdir=self.listdir, lstat=self.lstat, read=self.read
        )


def test_a_regular_file_named_review_md_is_read() -> None:
    disk = _Disk(["README.md", "REVIEW.md"], stat.S_IFREG | 0o644, FULL_TEXT)
    assert disk.problems() == []
    assert disk.reads == 1


def test_a_file_named_review_md_in_another_case_is_absent() -> None:
    for name in ("review.md", "Review.md", "REVIEW.MD"):
        disk = _Disk(["README.md", name], stat.S_IFREG | 0o644, FULL_TEXT)
        assert disk.problems() == ["REVIEW.md is absent"], name
        assert disk.reads == 0, name


def test_a_symbolic_link_named_review_md_is_not_a_regular_file() -> None:
    disk = _Disk(["REVIEW.md"], stat.S_IFLNK | 0o777, FULL_TEXT)
    assert disk.problems() == ["REVIEW.md is not a regular file"]
    assert disk.reads == 0


def test_an_empty_root_has_no_review_md() -> None:
    disk = _Disk([], stat.S_IFREG | 0o644, FULL_TEXT)
    assert disk.problems() == ["REVIEW.md is absent"]


def test_repository_review_md_has_its_shape() -> None:
    assert repository_problems(REPO_ROOT) == []

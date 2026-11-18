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


REPO_ROOT = Path(__file__).resolve().parents[1]
REVIEW_MD = REPO_ROOT / "REVIEW.md"

REQUIRED_HEADINGS = (
    "## What MAJOR means here",
    "## What not to report",
    "## Triggers (rules a diff can show)",
    "## Pruning",
)

PRUNE_BY = re.compile(r"^Prune by: (\d{4}-\d{2}-\d{2})$")


def review_md_problems(text: str | None) -> list[str]:
    """Return what is wrong with a REVIEW.md text; None means the file is absent."""
    if text is None:
        return ["REVIEW.md is absent"]
    lines = [line.rstrip() for line in text.splitlines()]
    problems = [
        f"missing heading {heading!r}"
        for heading in REQUIRED_HEADINGS
        if heading not in lines
    ]
    if not any(_prune_date(line) for line in lines):
        problems.append("no 'Prune by: YYYY-MM-DD' line with a real date")
    return problems


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

- Anything CI already enforces.

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


def test_repository_review_md_has_its_shape() -> None:
    text = REVIEW_MD.read_text(encoding="utf-8") if REVIEW_MD.is_file() else None
    assert review_md_problems(text) == []

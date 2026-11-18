"""REVIEW.md at the repository root keeps the shape its readers rely on.

Every reviewer of a pull request here reads REVIEW.md from the base branch
(git show main:REVIEW.md). This test holds the root to a regular file named
exactly REVIEW.md, and the file to four headings, the two sentences of "What
not to report" that no skip overrides, and a "Prune by: YYYY-MM-DD" line,
each counted only where the rendered file shows it. It never compares that date
with today, so a calendar date can never turn the suite red on an unrelated
pull request. It reads only the repository's own file and writes nothing.
"""

from __future__ import annotations

from collections.abc import Callable
import datetime
import os
from pathlib import Path
import re
import stat
from types import SimpleNamespace


REPO_ROOT = Path(__file__).resolve().parents[1]
NAME = "REVIEW.md"

REQUIRED_HEADINGS = (
    "## What MAJOR means here",
    "## What not to report",
    "## Triggers (rules a diff can show)",
    "## Pruning",
)

# The two sentences of "## What not to report" that no skip overrides. A
# list item there must begin with each, whitespace aside, so a change to
# either is made here as well as in REVIEW.md.
ALWAYS_SENTENCES = (
    "- Always reported: a secret, a key, a token, an injection or any other "
    "security fault in the diff, whatever CodeQL says; the scanner is a "
    "floor, not the reviewer's replacement.",
    "- Always reviewed by hand: a change to REVIEW.md itself, whatever CI runs "
    "on its pull request.",
)

PRUNE_BY = re.compile(r"^Prune by: (\d{4}-\d{2}-\d{2})$")
FENCE_OPEN = re.compile(r"^ {0,3}(`{3,}|~{3,})(.*)$")
COMMENT_OPEN = "<!--"
COMMENT_CLOSE = "-->"
LINE_END = re.compile(r"\r\n|\r|\n")
RAW_HTML_OPEN = re.compile(r"^ {0,3}<[A-Za-z/?!]")
AUTOLINK = re.compile(
    r"^ {0,3}<(?:[A-Za-z][A-Za-z0-9+.-]{1,31}:[^\s<>]*|[^\s<>@]+@[^\s<>]+)>"
)


def _read(path: Path) -> str:
    return path.read_text(encoding="utf-8")


def repository_problems(
    root: Path,
    *,
    listdir: Callable[[Path], list[str]] = os.listdir,
    lstat: Callable[[Path], os.stat_result] = os.lstat,
    read: Callable[[Path], str] = _read,
) -> list[str]:
    """Return what is wrong with REVIEW.md at a repository root.

    The gate reads git show main:REVIEW.md. A case-insensitive disk opens
    review.md by either name where git and a Linux runner find no file, and
    a symbolic link opens its target where git prints only the target's
    path, so only a regular file named exactly REVIEW.md counts.
    """
    if NAME not in listdir(root):
        return review_md_problems(None)
    if not stat.S_ISREG(lstat(root / NAME).st_mode):
        return ["REVIEW.md is not a regular file"]
    return review_md_problems(read(root / NAME))


def review_md_problems(text: str | None) -> list[str]:
    """Return what is wrong with a REVIEW.md text; None means the file is absent."""
    if text is None:
        return ["REVIEW.md is absent"]
    lines = visible_lines(text)
    problems = [
        f"missing heading {heading!r}"
        for heading in REQUIRED_HEADINGS
        if heading not in lines
    ]
    skips = REQUIRED_HEADINGS[1]
    if skips in lines:
        items = section_items(lines, skips)
        begins = [" ".join(item.split()) for item in items]
        for sentence in ALWAYS_SENTENCES:
            if not any(item.startswith(sentence) for item in begins):
                problems.append(f"{skips!r} has no item that begins {sentence!r}")
    if not any(_prune_date(line) for line in lines):
        problems.append("no 'Prune by: YYYY-MM-DD' line with a real date")
    return problems


def visible_lines(text: str) -> list[str]:
    """The file's lines, without trailing space, blank where the page hides them.

    Only a newline or a carriage return ends a line, as in CommonMark. A line
    inside a fenced code block or an HTML comment, and the lines that open
    and close one, come back blank. A fence closes only on a line of its own
    character, at least as long, with nothing after it. A fence or a comment
    never closed hides the rest of the file, and so does any other raw HTML
    block. A comment that opens and closes inside one line is cut out of that
    line. Where the page's rendering is in doubt the checker hides more than
    the page does, never less, so a doubtful file fails red, never green.
    """
    lines: list[str] = []
    fence: tuple[str, int] | None = None
    in_comment = False
    raw_html = False
    for line in LINE_END.split(text):
        line = line.rstrip()
        opened = None if fence or in_comment or raw_html else _fence_opened(line)
        if raw_html:
            lines.append("")
        elif fence is not None:
            if _closes(line, fence):
                fence = None
            lines.append("")
        elif in_comment:
            in_comment = COMMENT_CLOSE not in line
            lines.append("")
        elif opened is not None:
            fence = opened
            lines.append("")
        elif line.lstrip(" ").startswith(COMMENT_OPEN):
            after = line.split(COMMENT_OPEN, 1)[1]
            in_comment = COMMENT_CLOSE not in after
            lines.append("")
        elif RAW_HTML_OPEN.match(line) and not AUTOLINK.match(line):
            raw_html = True
            lines.append("")
        else:
            line, in_comment = _cut_comments(line)
            lines.append(line)
    return lines


def _fence_opened(line: str) -> tuple[str, int] | None:
    match = FENCE_OPEN.match(line)
    if match is None:
        return None
    run, info = match.groups()
    if run[0] == "`" and "`" in info:
        return None  # a code span on one line, not a fence
    return run[0], len(run)


def _closes(line: str, fence: tuple[str, int]) -> bool:
    char, length = fence
    bare = line.lstrip(" ")
    indent = len(line) - len(bare)
    return indent <= 3 and len(bare) >= length and bare == char * len(bare)


def _cut_comments(line: str) -> tuple[str, bool]:
    """The line without its comments, and whether a comment is left open."""
    kept = ""
    while COMMENT_OPEN in line:
        before, after = line.split(COMMENT_OPEN, 1)
        kept += before
        if COMMENT_CLOSE not in after:
            return kept.rstrip(), True
        line = after.split(COMMENT_CLOSE, 1)[1]
    return (kept + line).rstrip(), False


def section_items(lines: list[str], heading: str) -> list[str]:
    """The list items of a section, from its heading to the next "## " line.

    An item is a line starting "- " with the lines that continue it; a blank
    line, a heading or the next item ends it.
    """
    items: list[str] = []
    open_item = False
    for line in lines[lines.index(heading) + 1 :]:
        if line.startswith("## "):
            break
        if line.startswith("- "):
            items.append(line)
            open_item = True
        elif open_item and line.strip() and not line.startswith("#"):
            items[-1] += "\n" + line
        else:
            open_item = False
    return items


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
  security fault in the diff, whatever CodeQL says; the scanner is a
  floor, not the reviewer's replacement.
- Always reviewed by hand: a change to REVIEW.md itself, whatever CI runs
  on its pull request.

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


REPORTED_SENTENCE = (
    "- Always reported: a secret, a key, a token, an injection or any other "
    "security fault in the diff, whatever CodeQL says; the scanner is a "
    "floor, not the reviewer's replacement."
)
BY_HAND_SENTENCE = (
    "- Always reviewed by hand: a change to REVIEW.md itself, whatever CI runs "
    "on its pull request."
)
ALWAYS_REPORTED = (
    "- Always reported: a secret, a key, a token, an injection or any other\n"
    "  security fault in the diff, whatever CodeQL says; the scanner is a\n"
    "  floor, not the reviewer's replacement.\n"
)
BY_HAND = (
    "- Always reviewed by hand: a change to REVIEW.md itself, whatever CI runs\n"
    "  on its pull request.\n"
)
NO_ALWAYS_REPORTED = (
    f"'## What not to report' has no item that begins {REPORTED_SENTENCE!r}"
)
NO_BY_HAND = f"'## What not to report' has no item that begins {BY_HAND_SENTENCE!r}"
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


def test_an_always_item_begins_with_its_whole_sentence() -> None:
    for word in ("secret", "key", "token", "injection", "security"):
        item = ALWAYS_REPORTED.replace(f" {word}", " thing")
        assert item != ALWAYS_REPORTED, word
        text = _replaced(FULL_TEXT, ALWAYS_REPORTED, item)
        assert review_md_problems(text) == [NO_ALWAYS_REPORTED], word
    turned = (
        ALWAYS_REPORTED.replace("reported: a secret", "reported: never a secret"),
        "- Always reported: nothing; the monkey, the secretary, tokens,\n"
        "  injections and insecurity are fine.\n",
    )
    for item in turned:
        text = _replaced(FULL_TEXT, ALWAYS_REPORTED, item)
        assert review_md_problems(text) == [NO_ALWAYS_REPORTED], item
    for item in (
        "- Always reviewed by hand: nothing, not even REVIEW.md.\n",
        "- Always reviewed by hand: a change to REVIEW.md itself.\n",
    ):
        text = _replaced(FULL_TEXT, BY_HAND, item)
        assert review_md_problems(text) == [NO_BY_HAND], item


def test_an_always_item_may_wrap_anywhere_and_go_on_after_its_sentence() -> None:
    rewrapped = REPORTED_SENTENCE.replace(", a token,", ",\n    a token,") + "\n"
    longer = BY_HAND.replace("request.\n", "request. A removed rule is MAJOR.\n")
    text = _replaced(FULL_TEXT, ALWAYS_REPORTED + BY_HAND, rewrapped + longer)
    assert review_md_problems(text) == []


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


def test_a_lazy_continuation_line_stays_in_its_item() -> None:
    # CommonMark joins an unindented line to the item's paragraph above it.
    unindented = ALWAYS_REPORTED.replace("\n  security", "\nsecurity")
    assert review_md_problems(_replaced(FULL_TEXT, ALWAYS_REPORTED, unindented)) == []


def test_hidden_always_items_do_not_count() -> None:
    for before, after in (("<!--\n", "-->\n"), ("```\n", "```\n"), ("~~~\n", "~~~\n")):
        text = _replaced(
            FULL_TEXT, ALWAYS_REPORTED + BY_HAND, before + ALWAYS_REPORTED + BY_HAND + after
        )
        assert review_md_problems(text) == [NO_ALWAYS_REPORTED, NO_BY_HAND], before


def test_a_heading_that_ends_the_section_ends_its_items() -> None:
    for heading in ("# Appendix\n\n", " ## Appendix\n\n", "Appendix\n--------\n\n",
                    "Appendix\n===\n\n"):
        text = _replaced(
            FULL_TEXT, ALWAYS_REPORTED + BY_HAND, heading + ALWAYS_REPORTED + BY_HAND
        )
        assert review_md_problems(text) == [NO_ALWAYS_REPORTED, NO_BY_HAND], heading


def test_a_deeper_heading_inside_the_section_keeps_its_items() -> None:
    text = _replaced(
        FULL_TEXT, ALWAYS_REPORTED + BY_HAND, "### Always\n\n" + ALWAYS_REPORTED + BY_HAND
    )
    assert review_md_problems(text) == []


def test_a_fence_line_ending_in_a_no_break_space_does_not_close_the_fence() -> None:
    text = _replaced(
        FULL_TEXT,
        ALWAYS_REPORTED + BY_HAND,
        "~~~\n" + ALWAYS_REPORTED + BY_HAND + "~~~\u00a0\n",
    )
    assert review_md_problems(text) == [
        "missing heading '## Triggers (rules a diff can show)'",
        "missing heading '## Pruning'",
        NO_ALWAYS_REPORTED,
        NO_BY_HAND,
        "no 'Prune by: YYYY-MM-DD' line with a real date",
    ]


def test_a_shorter_fence_line_does_not_close_a_longer_fence() -> None:
    text = _pruning_wrapped("````\n```\n", "````\n")
    assert review_md_problems(text) == PRUNING_HIDDEN


def test_a_fence_line_with_text_after_it_does_not_close_the_fence() -> None:
    text = _pruning_wrapped("```\n```text\n", "```\n")
    assert review_md_problems(text) == PRUNING_HIDDEN


def test_an_unclosed_html_comment_hides_the_rest_of_the_file() -> None:
    assert review_md_problems(_pruning_wrapped("<!--\n", "")) == PRUNING_HIDDEN


def test_an_open_comment_inside_a_line_hides_the_rest_for_the_checker() -> None:
    # Stricter than the page, which shows "A note <!-- open" as text: where
    # the checker is unsure what renders, it hides more, so it fails red.
    assert review_md_problems(_pruning_wrapped("A note <!-- open\n", "")) == (
        PRUNING_HIDDEN
    )


def test_raw_html_hides_the_rest_of_the_file() -> None:
    openers = (
        "<?\n",
        "<?\n?>\n",
        "<![CDATA[\n",
        "<!X\n",
        "<div>\n",
        "  <DIV class=x>\n",
        "</div>\n",
        "<script>\n",
        "<details>\n\n",
        "<details>\n\n</details>\n\n",
    )
    for opener in openers:
        assert review_md_problems(_pruning_wrapped(opener, "")) == PRUNING_HIDDEN, opener
    for item in (ALWAYS_REPORTED, BY_HAND):
        text = _replaced(FULL_TEXT, item, "<?\n" + item + "?>\n")
        assert NO_ALWAYS_REPORTED in review_md_problems(text) or NO_BY_HAND in (
            review_md_problems(text)
        ), item


def test_an_autolink_at_a_line_start_is_not_raw_html() -> None:
    for link in ("<https://example.com/review>", "<owner@example.com>"):
        text = _pruning_wrapped(f"{link} has the page.\n\n", "")
        assert review_md_problems(text) == [], link


def test_only_a_newline_or_a_carriage_return_ends_a_line() -> None:
    for separator in ("\u2028", "\u2029", "\x0c", "\x0b", "\x1c", "\x85"):
        text = _replaced(FULL_TEXT, "\n## Pruning\n", f"{separator}## Pruning\n")
        assert review_md_problems(text) == ["missing heading '## Pruning'"], repr(
            separator
        )
    assert review_md_problems(FULL_TEXT.replace("\n", "\r")) == []


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

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
# A line that ends a level-two section on the page: an ATX heading of level
# one or two (up to three spaces before it), or a setext underline. A bare
# "-" or "---" line is read as one too, so a doubtful line ends the items.
SECTION_END = re.compile(r"^ {0,3}(?:#{1,2}(?:[ \t]|$)|=+[ \t]*$|-+[ \t]*$)")
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
    """The lines, trailing spaces and tabs cut, blank where the page hides them.

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
        line = line.rstrip(" \t")
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
    """The list items of a section, from its heading to the line that ends it.

    An item is a line starting "- " with the lines that continue it; a blank
    line, a heading or the next item ends it.
    """
    items: list[str] = []
    open_item = False
    for line in lines[lines.index(heading) + 1 :]:
        if SECTION_END.match(line):
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

A fault this file gives no other grade takes the grade of its effect. A
finding the reviewer cannot place is marked UNKNOWN and keeps the grade
beside it. It counts at that grade for the verdict, whatever label a brief
adds, until the mayor settles it.

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
        "Prune by: 2027-02-17.",
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
PAIR = (
    "A finding the reviewer cannot place is marked UNKNOWN and keeps the grade "
    "beside it. It counts at that grade for the verdict, whatever label a brief "
    "adds, until the mayor settles it."
)
PAIR_LINES = (
    "A fault this file gives no other grade takes the grade of its effect. A\n"
    "finding the reviewer cannot place is marked UNKNOWN and keeps the grade\n"
    "beside it. It counts at that grade for the verdict, whatever label a brief\n"
    "adds, until the mayor settles it.\n"
)
NO_PAIR = f"'## What MAJOR means here' has no paragraph with {PAIR!r}"
PRUNING = "## Pruning\n\nPrune by: 2027-02-17\n"


def _replaced(text: str, old: str, new: str) -> str:
    assert text.count(old) == 1, old
    return text.replace(old, new)


def _pruning_wrapped(before: str, after: str) -> str:
    return _replaced(FULL_TEXT, PRUNING, before + PRUNING + after)


def _line_number(text: str, line: str) -> int:
    numbers = [n for n, found in enumerate(text.splitlines(), 1) if found == line]
    assert numbers, line
    return numbers[0]


def _unread_problem(text: str, line: str, shown: str, what: str) -> str:
    return f"line {_line_number(text, line)} has {shown!r}: {what}, and the checker reads none"


HTML = "HTML or a comment can hide text on the page"
FENCE_LINE = "a code fence keeps the lines in it from reading as rules"
LINK_DEFINITION = "a link reference definition does not show on the page"
TABLE = "front matter shows as a table, not as rules"


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


def test_an_item_in_another_list_marker_or_with_a_typographic_apostrophe_counts() -> None:
    # "* " and "+ " open a list item as "- " does; the page shows a curly
    # apostrophe where a straight one was typed.
    for marker in ("* ", "+ "):
        text = FULL_TEXT.replace("\n- ", f"\n{marker}")
        assert review_md_problems(text) == [], marker
    curly = _replaced(FULL_TEXT, "reviewer's replacement", "reviewer’s replacement")
    assert review_md_problems(curly) == []


def test_a_heading_with_a_closing_sequence_counts() -> None:
    for heading in ("## Pruning ##", "## Pruning #####", "##\tPruning", " ## Pruning"):
        text = _replaced(FULL_TEXT, "## Pruning\n", heading + "\n")
        assert review_md_problems(text) == [], heading


def test_a_heading_that_ends_the_section_ends_its_items() -> None:
    for heading in ("# Appendix\n\n", " ## Appendix\n\n", "\nAppendix\n--------\n\n",
                    "\nAppendix\n===\n\n", "\nAppendix\n-\n\n"):
        text = _replaced(
            FULL_TEXT, ALWAYS_REPORTED + BY_HAND, heading + ALWAYS_REPORTED + BY_HAND
        )
        assert review_md_problems(text) == [NO_ALWAYS_REPORTED, NO_BY_HAND], heading


def test_a_rule_line_between_items_does_not_end_the_section() -> None:
    # A line of "-" after a blank line, or under an item's line, is a
    # thematic break; a line of "=" under an item's line goes on with it.
    for rule in ("\n---\n\n", "---\n", "\n***\n\n", "===\n", "\n- - -\n\n"):
        text = _replaced(
            FULL_TEXT, ALWAYS_REPORTED + BY_HAND, rule + ALWAYS_REPORTED + BY_HAND
        )
        assert review_md_problems(text) == [], rule


def test_a_deeper_heading_inside_the_section_keeps_its_items() -> None:
    text = _replaced(
        FULL_TEXT, ALWAYS_REPORTED + BY_HAND, "### Always\n\n" + ALWAYS_REPORTED + BY_HAND
    )
    assert review_md_problems(text) == []


def test_the_unknown_sentences_are_required_in_a_paragraph_of_their_section() -> None:
    assert review_md_problems(_replaced(FULL_TEXT, PAIR_LINES, "")) == [NO_PAIR]
    alone = PAIR.replace(" it. It", " it.\nIt") + "\n"
    assert review_md_problems(_replaced(FULL_TEXT, PAIR_LINES, alone)) == []
    elsewhere = _replaced(FULL_TEXT, PAIR_LINES, "")
    elsewhere = _replaced(elsewhere, "- Nothing private.\n", "- Nothing private.\n\n" + alone)
    assert review_md_problems(elsewhere) == [NO_PAIR]
    for shape in ("- " + alone, "> " + alone, "    " + alone, "Heading\n" + alone + "---\n"):
        text = _replaced(FULL_TEXT, PAIR_LINES, shape)
        assert review_md_problems(text) == [NO_PAIR], shape


def test_the_unknown_sentences_are_held_word_for_word_from_a_sentence_start() -> None:
    changed = (
        PAIR_LINES.replace("keeps the grade", "drops the grade"),
        PAIR_LINES.replace("whatever label a brief\nadds", "unless a brief\nsays"),
        PAIR_LINES.replace("It counts", "It never counts"),
        PAIR_LINES.replace("effect. A\nfinding", "effect. Never: a\nfinding"),
        PAIR_LINES.replace("effect. A\nfinding", "effect, unless a\nfinding"),
        PAIR_LINES.replace("beside it. It", "beside it.\n\nIt"),
    )
    for lines in changed:
        assert lines != PAIR_LINES, lines
        text = _replaced(FULL_TEXT, PAIR_LINES, lines)
        assert review_md_problems(text) == [NO_PAIR], lines


def test_comments_are_not_read() -> None:
    cases = (
        ("<!--\n", "-->\n", "<!--", "<!--"),
        ("<!-- a note -->\n\n", "", "<!-- a note -->", "<!-- a note -->"),
        ("A note <!-- open\n", "", "A note <!-- open", "<!-- open"),
        # Two comments on one line: the second opens where the first closes.
        ("<!-- note --> <!--\n", "-->\n", "<!-- note --> <!--", "<!-- note -->"),
        ("<!--\nold\n--> <!--\n", "-->\n", "<!--", "<!--"),
        ("Use the `<!--` marker.\n\n", "", "Use the `<!--` marker.", "<!--` marker."),
    )
    for before, after, line, shown in cases:
        text = _pruning_wrapped(before, after)
        problems = review_md_problems(text)
        assert problems[0] == _unread_problem(text, line, shown, HTML), before
        assert all(problem.startswith("line ") for problem in problems), before
    text = _replaced(FULL_TEXT, "## Pruning\n", "## Pruning <!-- a note -->\n")
    assert review_md_problems(text) == [
        _unread_problem(text, "## Pruning <!-- a note -->", "<!-- a note -->", HTML),
        "missing heading '## Pruning'",
    ]


def test_hidden_always_items_are_red() -> None:
    for before, after in (
        ("<!--\n", "-->\n"),
        ("<!-- note --> <!--\n", "-->\n"),
        ("```\n", "```\n"),
        ("~~~\n", "~~~\n"),
        ("<?\n", "?>\n"),
    ):
        text = _replaced(
            FULL_TEXT, ALWAYS_REPORTED + BY_HAND, before + ALWAYS_REPORTED + BY_HAND + after
        )
        problems = review_md_problems(text)
        number = _line_number(text, before.rstrip("\n"))
        assert problems[0].startswith(f"line {number} has "), before


def test_html_is_not_read() -> None:
    cases = (
        ("<?\n", "<?"),
        ("<![CDATA[\n", "<![CDATA["),
        ("<!X\n", "<!X"),
        ("<div>\n", "<div>"),
        ("  <DIV class=x>\n", "<DIV class=x>"),
        ("</div>\n", "</div>"),
        ("<script>\n", "<script>"),
        ("<details>\n\n</details>\n\n", "<details>"),
        ("<br>\n", "<br>"),
        ("Write to <account> here.\n\n", "<account>"),
    )
    for before, shown in cases:
        text = _pruning_wrapped(before, "")
        line = before.split("\n", 1)[0]
        assert review_md_problems(text)[0] == _unread_problem(text, line, shown, HTML), before
    # A tag that opens inside a line, above the two items it would fold away.
    folded = "- What a CI job itself prints. <details><summary>old</summary>\n"
    text = _replaced(FULL_TEXT, "- What a CI job itself prints.\n", folded)
    assert review_md_problems(text) == [
        _unread_problem(text, folded.rstrip("\n"), "<details>", HTML)
    ]


def test_an_autolink_is_read() -> None:
    for link in ("<https://example.com/review>", "<owner@example.com>"):
        for before in (f"{link} has the page.\n\n", f"The page is at {link} now.\n\n"):
            assert review_md_problems(_pruning_wrapped(before, "")) == [], before
    for not_a_link in ("<!--x@y-->", "<owner@example.com-->"):
        text = _pruning_wrapped(f"{not_a_link}\n\n", "")
        number = _line_number(text, not_a_link)
        assert review_md_problems(text)[0].startswith(f"line {number} has "), not_a_link


def test_code_fences_are_not_read() -> None:
    cases = (
        ("```\n", "```\n"),
        ("~~~\n", "~~~\n"),
        ("````\n```\n", "````\n"),
        ("```\n```text\n", "```\n"),
        ("~~~\n", "~~~ \n"),
        ("    ```\n", ""),
    )
    for before, after in cases:
        text = _pruning_wrapped(before, after)
        problems = review_md_problems(text)
        first = before.split("\n", 1)[0]
        assert problems[0] == _unread_problem(text, first, first.strip(), FENCE_LINE), before
        assert len(problems) == (before + after).count("\n"), before
        assert all(problem.startswith("line ") for problem in problems), before


def test_a_link_reference_definition_is_not_read() -> None:
    # Its title can run over lines, and the page shows none of it.
    hidden = "[x]: /u '\nPrune by: 2027-02-17\n'\n"
    text = _replaced(FULL_TEXT, "Prune by: 2027-02-17\n", hidden)
    assert review_md_problems(text) == [
        _unread_problem(text, "[x]: /u '", "[x]: /u '", LINK_DEFINITION)
    ]
    footnote = "[^1]: " + PAIR_LINES
    text = _replaced(FULL_TEXT, PAIR_LINES, footnote)
    first = footnote.splitlines()[0]
    assert review_md_problems(text) == [
        _unread_problem(text, first, first[:20], LINK_DEFINITION)
    ]


def test_front_matter_is_not_read() -> None:
    # A first line of "---" opens front matter: the page shows it as a
    # table, and a line in it that starts with "#" as nothing at all.
    text = "---\n## Pruning\nPrune by: 2027-02-17\n---\n" + _replaced(FULL_TEXT, PRUNING, "")
    assert review_md_problems(text) == [
        f"line 1 has '---': {TABLE}, and the checker reads none"
    ]


def test_only_a_newline_or_a_carriage_return_ends_a_line() -> None:
    for separator in (" ", " ", "\x0c", "\x0b", "\x1c", "\x85"):
        text = _replaced(FULL_TEXT, "\n## Pruning\n", f"{separator}## Pruning\n")
        assert review_md_problems(text) == ["missing heading '## Pruning'"], repr(
            separator
        )
    assert review_md_problems(FULL_TEXT.replace("\n", "\r")) == []


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

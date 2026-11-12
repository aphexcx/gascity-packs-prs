"""The texting rules: one catalog of reactions in the three channel texts.

The Google Chat skill, the WhatsApp skill and the Slack prompt fragment each
carry the same tables of reactions and the same four rules about them
(gp-adh3). The tables are the catalog, reactions.json, row for row and word
for word (gp-bis1; the catalog's own tests are tests/test_reaction_catalog.py).
A change to one text that is not made in the other two, or in the catalog,
fails here.
"""

import json
import re
import unicodedata
from pathlib import Path

import pytest


REPO = Path(__file__).resolve().parents[1]

TEXTS = {
    "gchat": REPO / "gchat" / "skills" / "google-chat" / "SKILL.md",
    "slack": REPO / "slack-full" / "template-fragments" / "slack-v0.template.md",
    "whatsapp": REPO / "whatsapp" / "skills" / "whatsapp" / "SKILL.md",
}

# The catalog's source. Its copies in the other packs equal it byte for byte
# (tests/test_reaction_catalog.py).
CATALOG = REPO / "gchat" / "reactions.json"

INTRO = "These reactions have fixed meanings, the same on Slack, Google Chat and WhatsApp."

# The sentence before each table: the first holds the rows an adapter may
# place, the second the rows that claim a finished fact.
TABLE_LEADS = (
    "Each one in the first table is true the moment a message arrives, so "
    "you or the adapter may place it:",
    "Each one in the second table states a finished fact, so it is yours to "
    "place, and only once the fact is true:",
)

TABLE_HEADER = "| Reaction | Slack name | What the owner reads it as |"

RULE_LEADS = [
    "- **A reaction comes first.**",
    "- **A reaction alone.**",
    "- **The second table is yours, and never a promise.**",
    "- **Fixed means fixed.**",
]


def read(name: str) -> str:
    return TEXTS[name].read_text(encoding="utf-8")


def normalized(text: str) -> str:
    return " ".join(text.split())


def catalog() -> list[dict]:
    return json.loads(CATALOG.read_text(encoding="utf-8"))["reactions"]


def table_line(row: dict) -> str:
    """A catalog row as a text's table shows it."""
    return f'| {row["emoji"]} | `{row["slack_name"]}` | "{row["meaning"]}" |'


def tables(text: str) -> list[list[str]]:
    """Each table under TABLE_HEADER, header first, exactly as the file has it."""
    lines = text.splitlines()
    found = []
    for start in (i for i, line in enumerate(lines) if line == TABLE_HEADER):
        rows = []
        for line in lines[start:]:
            if not line.startswith("|"):
                break
            rows.append(line)
        found.append(rows)
    return found


def reactions_shown(text: str) -> list[str]:
    """Every emoji the text shows: a pictographic symbol, with the emoji
    presentation selector when the text has one after it."""
    shown = []
    for i, ch in enumerate(text):
        if unicodedata.category(ch) == "So" and ord(ch) >= 0x2190:
            shown.append(ch + ("️" if text[i + 1 : i + 2] == "️" else ""))
    return shown


# The other bullets of the section the rules sit in, before the rules and
# after them. Google Chat keeps its rules inside "When to speak"; Slack and
# WhatsApp give them a "Reactions" section of their own. A bullet that is
# not named here is a rule, wherever in the section it is put.
OTHER_BULLETS = {
    "gchat": (
        ("- **DM or @mention:**", "- **Anything else in a space**"),
        ("- Reply once per message.", "- **Nobody reads your terminal.**"),
    ),
    "slack": ((), ()),
    "whatsapp": ((), ()),
}


BULLET = re.compile(r"([-*+]|\d+[.)]) ")

# A Markdown heading: one to six hashes, then a space or the end of the line.
HEADING = re.compile(r"#{1,6}( |$)")


def section_blocks(text: str) -> list[tuple[str, list[str]]]:
    """The section the rules sit in, as bullets with their lines, and other lines.

    The section runs from the heading above the first rule to the next
    heading, outside code blocks, so a bullet after a blank line or after a
    paragraph is still in it. Any list marker counts as a bullet (-, *, +,
    1.). A bullet's lines are the indented lines after it, across blank
    lines, and an unindented line straight after one of its lines, which
    Markdown reads as part of the same item. Every other non-blank line is
    its own ("other", [line]) block.
    """
    lines = text.splitlines()
    starts = [i for i, line in enumerate(lines) if line.startswith(RULE_LEADS[0])]
    assert len(starts) == 1, f"expected the first rule once, found {len(starts)}"
    first = starts[0]
    headings, fence = [], False
    for i, line in enumerate(lines):
        if line.startswith("```"):
            fence = not fence
        elif not fence and HEADING.match(line):
            headings.append(i)
    top = max((i for i in headings if i < first), default=-1)
    bottom = min((i for i in headings if i > first), default=len(lines))
    blocks: list[tuple[str, list[str]]] = []
    fence, inside_item, after_blank = False, False, False
    for line in lines[top + 1 : bottom]:
        if line.startswith("```"):
            fence, inside_item = not fence, False
            blocks.append(("other", [line]))
        elif fence:
            blocks[-1][1].append(line)
        elif not line.strip():
            after_blank = True
            continue
        elif BULLET.match(line):
            blocks.append(("bullet", [line]))
            inside_item = True
        elif inside_item and (line.startswith("  ") or not after_blank):
            blocks[-1][1].append(line)
        else:
            blocks.append(("other", [line]))
            inside_item = False
        after_blank = False
    return blocks


def section_items(text: str) -> list[list[str]]:
    """Each bullet of the section the rules sit in, with its lines."""
    return [lines for kind, lines in section_blocks(text) if kind == "bullet"]


def is_other_bullet(name: str, item: list[str]) -> bool:
    before, after = OTHER_BULLETS[name]
    return item[0].startswith(before + after)


def rules(name: str, text: str) -> list[list[str]]:
    """The rules of one text: every bullet of their section but the text's other bullets."""
    return [item for item in section_items(text) if not is_other_bullet(name, item)]


def bullet_problems(name: str, text: str) -> list[str]:
    """The section's bullets, against the text's other bullets around the four leads.

    The rules are also one list: nothing but rule bullets stands between the
    first rule and the last.
    """
    before, after = OTHER_BULLETS[name]
    expected = [*before, *RULE_LEADS, *after]
    found = [item[0] for item in section_items(text)]
    if len(found) != len(expected) or not all(
        line.startswith(lead) for line, lead in zip(found, expected)
    ):
        return [f"{name}: the section's bullets are {found}"]
    blocks = section_blocks(text)
    is_rule = [kind == "bullet" and not is_other_bullet(name, lines) for kind, lines in blocks]
    first, last = is_rule.index(True), len(is_rule) - 1 - is_rule[::-1].index(True)
    between = [blocks[i][1] for i in range(first, last + 1) if not is_rule[i]]
    if between:
        return [f"{name}: lines among the rules: {between}"]
    return []


def rule_problems(name: str, text: str) -> list[str]:
    """What is wrong with the rules of one text: its bullets, and Google Chat's rules."""
    problems = bullet_problems(name, text)
    if rules(name, text) != rules("gchat", read("gchat")):
        problems.append(f"{name}: the rules differ from gchat")
    return problems


FIFTH_RULE = "- **A fifth rule.** Nobody agreed to this one."

FIFTH_RULE_PLACES = (
    "after a blank line at the end of the list",
    "at the end of the section",
    "before the first rule",
)


def with_a_fifth_rule(text: str, place: str) -> str:
    """The text with one more rule bullet, put in the section the rules sit in.

    It finds its places on its own, without the helpers the checks use.
    """
    lines = text.splitlines()
    first = next(i for i, line in enumerate(lines) if line.startswith(RULE_LEADS[0]))
    end = next(i for i, line in enumerate(lines) if line.startswith(RULE_LEADS[-1]))
    while end + 1 < len(lines) and lines[end + 1].startswith("  "):
        end += 1
    fence, section_end = False, len(lines)
    for i, line in enumerate(lines):
        if line.startswith("```"):
            fence = not fence
        elif not fence and i > first and HEADING.match(line):
            section_end = i
            break
    if place == "after a blank line at the end of the list":
        lines[end + 1 : end + 1] = ["", FIFTH_RULE]
    elif place == "at the end of the section":
        lines[section_end:section_end] = [FIFTH_RULE, ""]
    elif place == "before the first rule":
        lines[first:first] = [FIFTH_RULE]
    else:
        raise ValueError(place)
    return "\n".join(lines) + "\n"


# The react commands of the three texts, and the flags that name one message.
# WhatsApp has no react verb yet; the bead that adds one adds it here.
REACT_COMMANDS = {
    "gc slack react": ("--conversation-id", "--message-id"),
    "gc gchat react": ("--message",),
}

# The one sentence form that names a react command without running it.
REACT_REFERENCES = tuple(f"When `{command}` fails," for command in REACT_COMMANDS)


def react_units(text: str) -> list[tuple[bool, str]]:
    """The text as (in_code, unit) pairs: each command of a code block, its
    backslash-continued lines joined, and each prose paragraph on one line."""
    units: list[tuple[bool, str]] = []
    fence, paragraph, command = False, [], ""
    for line in text.splitlines() + [""]:
        if line.startswith("```"):
            if paragraph:
                units.append((False, " ".join(paragraph)))
                paragraph = []
            fence = not fence
        elif fence:
            command += line.rstrip("\\").strip() + " "
            if not line.endswith("\\"):
                units.append((True, command.strip()))
                command = ""
        elif line.strip():
            paragraph.append(line.strip())
        elif paragraph:
            units.append((False, " ".join(paragraph)))
            paragraph = []
    return units


def react_uses(text: str) -> list[str]:
    """Every place a text writes a react command, with what follows it.

    In a code block that is the rest of its command; in prose, the rest of
    the code span or, outside one, the rest of the paragraph. The command
    named inside one of REACT_REFERENCES is a reference, not a use.
    """
    uses = []
    for in_code, unit in react_units(text):
        references = [
            (m.start(), m.end())
            for ref in REACT_REFERENCES
            for m in re.finditer(re.escape(ref), unit)
        ]
        for command in REACT_COMMANDS:
            for m in re.finditer(re.escape(command), unit):
                if any(a <= m.start() < b for a, b in references):
                    continue
                rest = unit[m.start() :]
                if not in_code and unit[: m.start()].count("`") % 2 == 1:
                    rest = rest.split("`", 1)[0]
                uses.append(rest)
    return uses


@pytest.mark.parametrize("name", sorted(TEXTS))
def test_each_text_carries_the_whole_catalog_in_its_two_tables(name: str) -> None:
    """The first table is every row an adapter may place, the second every
    claim, each in the catalog's order, word for word (gp-bis1)."""
    found = tables(read(name))
    assert len(found) == 2, f"{name}: expected two tables, found {len(found)}"
    rows = catalog()
    for table, may_place in zip(found, (True, False)):
        assert table[1] == "| --- | --- | --- |", table
        assert table[2:] == [table_line(row) for row in rows if row["adapter_may_place"] is may_place]


def test_the_tables_are_the_same_in_every_text() -> None:
    gchat = tables(read("gchat"))
    for name in ("slack", "whatsapp"):
        assert tables(read(name)) == gchat, f"{name} differs from gchat"


@pytest.mark.parametrize("name", sorted(TEXTS))
def test_every_reaction_a_text_shows_is_a_catalog_row(name: str) -> None:
    """A reaction in a rule, an example or a table is one of the catalog's,
    in the catalog's own form (with the emoji presentation selector or without)."""
    emoji = {row["emoji"] for row in catalog()}
    stray = sorted(set(reactions_shown(read(name))) - emoji)
    assert not stray, f"{name} shows reactions that are not catalog rows: {stray}"


@pytest.mark.parametrize("name", sorted(TEXTS))
def test_the_intro_and_the_two_table_leads_come_in_order(name: str) -> None:
    text = read(name)
    flat = normalized(text)
    marks = [flat.index(INTRO), *(flat.index(lead) for lead in TABLE_LEADS)]
    assert marks == sorted(marks)
    lines = text.splitlines()
    headers = [i for i, line in enumerate(lines) if line == TABLE_HEADER]
    first_rule = next(i for i, line in enumerate(lines) if line.startswith(RULE_LEADS[0]))
    for lead, header in zip(TABLE_LEADS, headers):
        # The lead is the paragraph straight above its table.
        above = []
        for line in reversed(lines[: header - 1]):
            if not line.strip():
                break
            above.insert(0, line)
        assert lines[header - 1] == "" and normalized("\n".join(above)).endswith(lead), (name, lead)
    assert headers[-1] < first_rule


@pytest.mark.parametrize("name", sorted(TEXTS))
def test_no_text_counts_the_reactions(name: str) -> None:
    """The texts stop saying five (gp-bis1): a count goes stale with the catalog."""
    assert not re.search(r"\b(five|the first three)\b", read(name), re.IGNORECASE)


@pytest.mark.parametrize(
    ("probe", "stray"),
    [
        ("React with 🙏 when they thank you.\n", ["🙏"]),
        ("React with ❤ when they thank you.\n", ["❤"]),
        ("React with ❤️ when they thank you.\n", []),
        ("Copyright ™ and © are not reactions.\n", []),
    ],
)
def test_the_scan_finds_a_reaction_outside_the_catalog(probe: str, stray: list[str]) -> None:
    emoji = {row["emoji"] for row in catalog()}
    assert sorted(set(reactions_shown(probe)) - emoji) == stray


@pytest.mark.parametrize("name", sorted(TEXTS))
def test_each_text_carries_the_four_rules_and_no_other(name: str) -> None:
    text = read(name)
    assert INTRO in normalized(text)
    assert bullet_problems(name, text) == []
    assert [item[0][: len(lead)] for item, lead in zip(rules(name, text), RULE_LEADS)] == RULE_LEADS


def test_the_rules_are_the_same_in_every_text() -> None:
    gchat = rules("gchat", read("gchat"))
    assert len(gchat) == len(RULE_LEADS)
    for name in ("slack", "whatsapp"):
        assert rules(name, read(name)) == gchat, f"{name} differs from gchat"


@pytest.mark.parametrize("place", FIFTH_RULE_PLACES)
@pytest.mark.parametrize("name", sorted(TEXTS))
def test_a_fifth_rule_anywhere_in_the_section_fails(name: str, place: str) -> None:
    """A rule added anywhere in the section the rules sit in fails (gp-1v3u).

    The round-1 read added a fifth rule after a blank line, and every test
    passed: Markdown reads that bullet as one more item of the same list.
    """
    assert bullet_problems(name, read(name)) == []
    probe = with_a_fifth_rule(read(name), place)
    assert FIFTH_RULE in probe.splitlines()
    assert rule_problems(name, probe), f"a fifth rule {place} in {name} passes"


def names_its_message(use: str) -> bool:
    command = next(c for c in REACT_COMMANDS if use.startswith(c))
    return all(
        f"{flag} " in use or f"{flag}=" in use for flag in REACT_COMMANDS[command]
    )


@pytest.mark.parametrize("name", sorted(TEXTS))
def test_every_react_command_names_its_message(name: str) -> None:
    """A reaction lands on the message the mayor answers, or is not placed (gp-1v3u).

    gc slack react with no ids reacts to the latest inbound it can find for
    the session. That lookup never sees a company room, DM or group DM turn,
    and on any turn it can pick a newer message. The reaction then lands on
    another message, the receipt says delivered, and the rule "no text
    follows" leaves the person with nothing. No kind of turn is safe for
    that form, so every example and instruction names the message.
    """
    uses = react_uses(read(name))
    bare = [use for use in uses if not names_its_message(use)]
    assert not bare, f"{name}: a react command that does not name its message: {bare}"


@pytest.mark.parametrize(
    "probe",
    [
        "```\ngc slack react [--emoji NAME] [--session SID]\n```\n",
        "React first: `gc slack react --emoji eyes`.\n",
        "Run `gc slack react` on the message.\n",
        "React with gc slack react --emoji mag before you start.\n",
        "```\ngc slack react --conversation-id Cxxx --emoji eyes\n```\n",
        "React with `gc gchat react --emoji 👀`.\n",
        "React first with `gc slack\nreact --emoji eyes`.\n",
        "React first with gc slack\nreact --emoji eyes.\n",
        "Run `gc gchat react` first. When `gc gchat react` fails, say it in words.\n",
    ],
)
def test_the_react_check_catches_a_form_that_names_no_message(probe: str) -> None:
    uses = react_uses(probe)
    assert uses, probe
    assert not all(names_its_message(use) for use in uses), probe


@pytest.mark.parametrize(
    "fine",
    [
        "When `gc slack react` fails, the reaction was not placed.\n",
        "When `gc gchat\nreact` fails, the reaction was not placed.\n",
        "```\ngc slack react --conversation-id Cxxx \\\n  --message-id 1234.5678 --emoji eyes\n```\n",
        "Run `gc slack react --conversation-id=Cxxx --message-id=1234.5678 --emoji eyes`.\n",
        "React with `gc gchat react --message spaces/A/messages/B --emoji 👀`.\n",
    ],
)
def test_the_react_check_passes_a_form_that_names_its_message(fine: str) -> None:
    assert all(names_its_message(use) for use in react_uses(fine)), fine


LINES_AMONG_THE_RULES = (
    "an unindented line straight after rule 2",
    "a paragraph between rules 2 and 3",
    "a starred bullet at the end of the section",
    "a numbered bullet after a blank line at the end of the list",
)


def with_a_line_among_the_rules(text: str, place: str) -> str:
    lines = text.splitlines()
    second = next(i for i, line in enumerate(lines) if line.startswith(RULE_LEADS[1]))
    third = next(i for i, line in enumerate(lines) if line.startswith(RULE_LEADS[2]))
    end = next(i for i, line in enumerate(lines) if line.startswith(RULE_LEADS[-1]))
    while end + 1 < len(lines) and lines[end + 1].startswith("  "):
        end += 1
    section_end = next(
        (i for i in range(end + 1, len(lines)) if HEADING.match(lines[i])), len(lines)
    )
    if place == "an unindented line straight after rule 2":
        lines[third:third] = ["Except in a DM, where a line of text follows."]
    elif place == "a paragraph between rules 2 and 3":
        lines[third:third] = ["", "Except in a DM, where a line of text follows.", ""]
    elif place == "a starred bullet at the end of the section":
        lines[section_end:section_end] = ["* **A fifth rule.** Nobody agreed to this one.", ""]
    elif place == "a numbered bullet after a blank line at the end of the list":
        lines[end + 1 : end + 1] = ["", "1. **A fifth rule.** Nobody agreed to this one."]
    else:
        raise ValueError(place)
    return "\n".join(lines) + "\n"


@pytest.mark.parametrize("place", LINES_AMONG_THE_RULES)
@pytest.mark.parametrize("name", sorted(TEXTS))
def test_a_line_slipped_among_the_rules_fails(name: str, place: str) -> None:
    """Lines Markdown reads into the rules, or that split them, fail (gp-1v3u self-review)."""
    assert bullet_problems(name, read(name)) == []
    probe = with_a_line_among_the_rules(read(name), place)
    assert probe != read(name)
    assert rule_problems(name, probe), f"{place} in {name} passes"


WHATSAPP_THANKS = (
    "Until you can, a message that only thanks, confirms, agrees or closes a "
    "topic still gets an answer from you in words (a reaction from them still "
    "rarely needs one)."
)


def test_whatsapp_says_what_a_thanks_gets_until_the_mayor_can_react() -> None:
    """Rules 1 and 2 hold only where the mayor can react (gp-1v3u, the round-1 read's story 2)."""
    flat = normalized(read("whatsapp"))
    cannot = flat.index("You cannot place a reaction on WhatsApp yet.")
    assert WHATSAPP_THANKS in flat
    assert cannot < flat.index(WHATSAPP_THANKS) < flat.index(RULE_LEADS[0])


@pytest.mark.parametrize(
    ("name", "failure"),
    [
        ("gchat", "When `gc gchat react` fails,"),
        ("slack", 'The command prints a receipt. When it shows `"delivered": false`,'),
    ],
)
def test_a_reaction_that_is_not_placed_is_said_in_words(name: str, failure: str) -> None:
    """The rules make a reaction the whole answer, so a failed one must not be silent.

    gc slack react exits 0 with "delivered": false when Slack refuses the
    reaction (a company DM the switchboard token cannot see, a message that
    is gone); gc gchat react exits non-zero with the HTTP error.
    """
    flat = normalized(read(name))
    assert f"{failure} the reaction was not placed, so say the same thing in words." in flat


def test_google_chat_skill_has_the_table_and_no_free_choice() -> None:
    text = read("gchat")
    flat = normalized(text)
    assert TABLE_HEADER in text
    assert "whatever emoji fits" not in flat
    assert "--emoji 👀" in text


def test_whatsapp_skill_says_the_mayor_cannot_react_there_yet() -> None:
    flat = normalized(read("whatsapp"))
    assert "You cannot place a reaction on WhatsApp yet." in flat
    assert "The adapter may place one from the first table when a message arrives." in flat
    assert "the user" not in flat.lower()


def test_the_older_short_reply_lines_stay() -> None:
    """Afik dropped the length rule (11/12); this bead removes none either.

    A founder's ruling may remove these lines later; change this test with it.
    """
    gchat = normalized(read("gchat"))
    assert "Keep replies short and plain; Chat is conversation, not a report." in gchat
    whatsapp = read("whatsapp")
    assert (
        'Answer promptly, even if it is only "on it". The owner sees the blue ticks\n'
        'and "typing…" as soon as the message reaches you, so silence after that\n'
        "reads as being ignored. Answer in the language they write in.\n"
    ) in whatsapp
    assert "## Write for a phone\n\n- Short: a few lines, one question at a time.\n" in whatsapp

"""The texting rules: one vocabulary of five reactions in the three channel texts.

The Google Chat skill, the WhatsApp skill and the Slack prompt fragment each
carry the same table of five reactions and the same four rules about them
(gp-adh3). A change to one text that is not made in the other two fails here.
"""

from pathlib import Path

import pytest


REPO = Path(__file__).resolve().parents[1]

TEXTS = {
    "gchat": REPO / "gchat" / "skills" / "google-chat" / "SKILL.md",
    "slack": REPO / "slack-full" / "template-fragments" / "slack-v0.template.md",
    "whatsapp": REPO / "whatsapp" / "skills" / "whatsapp" / "SKILL.md",
}

INTRO = "Five reactions have fixed meanings, the same on Slack, Google Chat and WhatsApp:"

TABLE_HEADER = "| Reaction | Slack name | What the owner reads it as |"

FIVE = [
    ("👀", "`eyes`"),
    ("🔍", "`mag`"),
    ("👍", "`+1`"),
    ("✅", "`white_check_mark`"),
    ("⏰", "`alarm_clock`"),
]

RULE_LEADS = [
    "- **A reaction comes first.**",
    "- **A reaction alone.**",
    "- **✅ and ⏰ are yours, and never a promise.**",
    "- **Fixed means fixed.**",
]


def read(name: str) -> str:
    return TEXTS[name].read_text(encoding="utf-8")


def normalized(text: str) -> str:
    return " ".join(text.split())


def table_rows(text: str) -> list[str]:
    """The table's lines, header first, exactly as the file has them."""
    lines = text.splitlines()
    starts = [i for i, line in enumerate(lines) if line == TABLE_HEADER]
    assert len(starts) == 1, f"expected the table header once, found {len(starts)}"
    rows = []
    for line in lines[starts[0] :]:
        if not line.startswith("|"):
            break
        rows.append(line)
    return rows


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


def section_items(text: str) -> list[list[str]]:
    """Each bullet of the section the rules sit in, with its wrapped lines.

    The section runs from the heading above the first rule to the next
    heading, outside code blocks, so a bullet after a blank line or after a
    paragraph is still in it. A bullet's wrapped lines are the indented
    lines after it, across blank lines, up to a line that is neither.
    """
    lines = text.splitlines()
    starts = [i for i, line in enumerate(lines) if line.startswith(RULE_LEADS[0])]
    assert len(starts) == 1, f"expected the first rule once, found {len(starts)}"
    first = starts[0]
    headings, fence = [], False
    for i, line in enumerate(lines):
        if line.startswith("```"):
            fence = not fence
        elif not fence and line.startswith("#"):
            headings.append(i)
    top = max((i for i in headings if i < first), default=-1)
    bottom = min((i for i in headings if i > first), default=len(lines))
    items: list[list[str]] = []
    fence, inside_item = False, False
    for line in lines[top + 1 : bottom]:
        if line.startswith("```"):
            fence, inside_item = not fence, False
        elif fence:
            continue
        elif line.startswith("- "):
            items.append([line])
            inside_item = True
        elif line.startswith("  ") and inside_item:
            items[-1].append(line)
        elif line.strip():
            inside_item = False
    return items


def rules(name: str, text: str) -> list[list[str]]:
    """The rules of one text: every bullet of their section but the text's other bullets."""
    before, after = OTHER_BULLETS[name]
    return [item for item in section_items(text) if not item[0].startswith(before + after)]


def bullet_problems(name: str, text: str) -> list[str]:
    """The section's bullets, against the text's other bullets around the four leads."""
    before, after = OTHER_BULLETS[name]
    expected = [*before, *RULE_LEADS, *after]
    found = [item[0] for item in section_items(text)]
    if len(found) != len(expected) or not all(
        line.startswith(lead) for line, lead in zip(found, expected)
    ):
        return [f"{name}: the section's bullets are {found}"]
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
        elif not fence and i > first and line.startswith("#"):
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

# Sentences that name a react command without running it.
REACT_REFERENCES = ("When `gc gchat react` fails,",)


def react_uses(text: str) -> list[str]:
    """Every place a text writes a react command, with what follows it.

    In a code block that is the rest of its line; in prose, the rest of the
    code span or, outside one, the rest of the line.
    """
    uses = []
    fence = False
    for line in text.splitlines():
        if line.startswith("```"):
            fence = not fence
            continue
        for command in REACT_COMMANDS:
            start = line.find(command)
            while start != -1:
                rest = line[start:]
                if not fence and line[:start].count("`") % 2 == 1:
                    rest = rest.split("`", 1)[0]
                if not any(ref in line for ref in REACT_REFERENCES) or rest != command:
                    uses.append(rest)
                start = line.find(command, start + 1)
    return uses


@pytest.mark.parametrize("name", sorted(TEXTS))
def test_each_text_carries_the_five_in_order(name: str) -> None:
    rows = table_rows(read(name))
    assert rows[1] == "| --- | --- | --- |"
    body = rows[2:]
    assert len(body) == 5, body
    for row, (emoji, slack_name) in zip(body, FIVE):
        cells = [cell.strip() for cell in row.strip("|").split("|")]
        assert cells[:2] == [emoji, slack_name], row
        assert cells[2].startswith('"') and cells[2].endswith('"'), row


def test_the_five_rows_are_the_same_in_every_text() -> None:
    gchat = table_rows(read("gchat"))
    for name in ("slack", "whatsapp"):
        assert table_rows(read(name)) == gchat, f"{name} differs from gchat"


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
    assert rule_problems(name, read(name)) == []
    probe = with_a_fifth_rule(read(name), place)
    assert FIFTH_RULE in probe.splitlines()
    assert rule_problems(name, probe), f"a fifth rule {place} in {name} passes"


def names_its_message(use: str) -> bool:
    command = next(c for c in REACT_COMMANDS if use.startswith(c))
    return all(f"{flag} " in use for flag in REACT_COMMANDS[command])


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
    ],
)
def test_the_react_check_catches_a_form_that_names_no_message(probe: str) -> None:
    uses = react_uses(probe)
    assert uses, probe
    assert not all(names_its_message(use) for use in uses), probe


WHATSAPP_THANKS = (
    "Until you can, a message that only thanks, confirms, agrees or closes a "
    "topic still gets an answer from you in words."
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
    assert "The adapter may place one of the first three when a message arrives." in flat
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

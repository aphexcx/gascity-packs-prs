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


def rules_block(text: str) -> list[str]:
    """The list of rules, from the first lead to the end of the list.

    The list runs on through bullets, their wrapped lines, and any indented
    paragraph inside a bullet. It ends at a line that is neither, or at a
    blank line that is not followed by an indented line.
    """
    lines = text.splitlines()
    starts = [i for i, line in enumerate(lines) if line.startswith(RULE_LEADS[0])]
    assert len(starts) == 1, f"expected the first rule once, found {len(starts)}"
    block = []
    rest = lines[starts[0] :]
    for i, line in enumerate(rest):
        if not line.strip():
            following = rest[i + 1] if i + 1 < len(rest) else ""
            if not following.startswith("  "):
                break
        elif not line.startswith(("- ", "  ")):
            break
        block.append(line)
    return block


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
    bullets = [line for line in rules_block(text) if line.startswith("- ")]
    assert len(bullets) == len(RULE_LEADS), bullets
    for bullet, lead in zip(bullets, RULE_LEADS):
        assert bullet.startswith(lead), bullet


def test_the_rules_are_the_same_in_every_text() -> None:
    gchat = rules_block(read("gchat"))
    for name in ("slack", "whatsapp"):
        assert rules_block(read(name)) == gchat, f"{name} differs from gchat"


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

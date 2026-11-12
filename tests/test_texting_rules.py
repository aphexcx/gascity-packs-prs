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


def rule(text: str, lead: str) -> str:
    """One rule bullet with its wrapped lines, exactly as the file has them."""
    lines = text.splitlines()
    starts = [i for i, line in enumerate(lines) if line.startswith(lead)]
    assert len(starts) == 1, f"expected {lead!r} once, found {len(starts)}"
    bullet = [lines[starts[0]]]
    for line in lines[starts[0] + 1 :]:
        if not line.startswith("  ") or not line.strip():
            break
        bullet.append(line)
    return "\n".join(bullet)


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


@pytest.mark.parametrize("lead", RULE_LEADS)
def test_each_rule_is_the_same_in_every_text(lead: str) -> None:
    gchat = rule(read("gchat"), lead)
    for name in ("slack", "whatsapp"):
        assert rule(read(name), lead) == gchat, f"{name} differs from gchat"


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

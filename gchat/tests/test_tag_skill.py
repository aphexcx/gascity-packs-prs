"""The reply guide must say whom an agent tags, and how a tag is written."""
import re
from pathlib import Path


def skill_text():
    return (Path(__file__).resolve().parents[1] / "skills/google-chat/SKILL.md").read_text()


def tag_part():
    section = skill_text().split("## How to reply", 1)[1].split("\n## ", 1)[0]
    return section.split("**Whom to tag.**", 1)[1]


def test_reply_section_says_whom_to_tag():
    section = skill_text().split("## How to reply", 1)[1].split("\n## ", 1)[0]
    words = " ".join(section.split())
    phrases = (
        "**Whom to tag.** Tag the one person whose answer or action your message needs."
        " Tag nobody when your message asks nothing of anyone.",
        "<users/1234567890>",
        "--body '<users/1234567890> ",
        "not as a typed `@Name` or an email address",
    )
    missing = [phrase for phrase in phrases if phrase not in words]
    assert not missing, missing


def test_every_tag_the_skill_writes_uses_a_numeric_id():
    # The send path converts Markdown: an email address holding a pair of
    # underscores would be rewritten inside the tag. A numeric id cannot be.
    tags = re.findall(r"<users/([^>]*)>", tag_part())
    assert tags, "the tag part writes no tag"
    assert all(tag == "ID" or tag.isdigit() for tag in tags), tags
    assert "stand in for ID" not in " ".join(tag_part().split())


def test_skill_gives_no_tag_of_everyone():
    assert "users/all" not in skill_text()

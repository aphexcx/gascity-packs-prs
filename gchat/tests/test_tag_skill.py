"""The reply guide must say whom an agent tags, and how a tag is written."""
from pathlib import Path


def skill_text():
    return (Path(__file__).resolve().parents[1] / "skills/google-chat/SKILL.md").read_text()


def test_reply_section_says_whom_to_tag():
    section = skill_text().split("## How to reply", 1)[1].split("\n## ", 1)[0]
    words = " ".join(section.split())
    phrases = (
        "Tag the one person whose answer or action your message needs.",
        "Tag nobody when your message asks nothing of anyone.",
        "<users/1234567890>",
    )
    missing = [phrase for phrase in phrases if phrase not in words]
    assert not missing, missing


def test_skill_gives_no_tag_of_everyone():
    assert "users/all" not in skill_text()

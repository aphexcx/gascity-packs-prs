"""The room approval ask must carry the facts the owner is deciding on."""
from pathlib import Path


def test_join_skill_names_the_owner_decision():
    text = (Path(__file__).resolve().parents[1] / "skills/google-chat/SKILL.md").read_text()
    section = text.split("## Accepting a room invitation", 1)[1].split("\n## ", 1)[0]
    for phrase in ("room's name", "name and address", "outside", "read", "unless", "yes", "direct message", "code", "24 hours", "no answer", "--request", "--yes"):
        assert phrase in section, phrase

"""The follow-through skill against the pack it teaches.

The skill is the mayor's only guide to when it may write to an owner, so it
must name the verbs the pack ships and no others, and it must keep the two
rules the founders rule on word for word.
"""
import os
from pathlib import Path
import re
import shutil
import subprocess
import tomllib

PACK = Path(__file__).resolve().parents[1]
SKILL = PACK / "skills/follow-through/SKILL.md"

# A change to either sentence is a founder's decision, not an edit.
NO_SECOND_FOLLOW_UP = "There is no second follow-up, ever."
MAJOR_FEATURE = "A note about a new ability goes out only when you were given one for a major feature."

VERB = re.compile(r"gc follow-through ([a-z][a-z-]*)([^`\n]*)")
FLAG = re.compile(r"--[a-z][a-z-]*")


def skill_text() -> str:
    return SKILL.read_text(encoding="utf-8")


def shipped_verbs() -> dict[str, str]:
    """Each command the pack ships, with the description gc shows as its help."""
    return {path.parent.name: tomllib.loads(path.read_text())["description"]
            for path in (PACK / "commands").glob("*/command.toml")}


def verb_flags() -> dict[str, set[str]]:
    """The flags each shipped command's own usage line names."""
    node = shutil.which("node")
    assert node, "follow-through requires Node.js 20 or newer"
    env = {**os.environ, "GC_PACK_STATE_DIR": "/nonexistent/follow-through", "GC_BIN": "/no-real-gc-allowed"}
    flags = {}
    for verb in shipped_verbs():
        usage = subprocess.run([str(PACK / "commands" / f"{verb}.sh"), "--help"], env=env,
                               capture_output=True, text=True, check=True, timeout=60).stdout
        assert usage.strip(), f"{verb} printed no usage"
        flags[verb] = set(FLAG.findall(usage))
    return flags


def test_skill_front_matter_names_it_follow_through():
    match = re.match(r"\A---\n(?P<body>.*?)\n---\n", skill_text(), re.DOTALL)
    assert match, "the skill has no front matter"
    fields = dict(line.split(":", 1) for line in match.group("body").splitlines())
    assert fields["name"].strip() == "follow-through"
    assert fields["description"].strip()


def test_skill_names_every_shipped_verb_and_no_other():
    named = {verb for verb, _ in VERB.findall(skill_text())}
    shipped = set(shipped_verbs())
    assert not shipped - named, f"verbs the pack ships that the skill never names: {sorted(shipped - named)}"
    assert not named - shipped, f"verbs the skill names that the pack does not ship: {sorted(named - shipped)}"


def test_verbs_list_gives_each_verb_its_help_line():
    section = skill_text().split("\n## The verbs\n", 1)
    assert len(section) == 2, "the skill has no section headed The verbs"
    listed = sorted(line for line in section[1].splitlines() if line.startswith("- "))
    assert listed == sorted(f"- `gc follow-through {verb}`: {description}."
                            for verb, description in shipped_verbs().items())


def test_every_flag_in_the_skill_is_one_its_verb_takes():
    flags = verb_flags()
    for verb, rest in VERB.findall(skill_text()):
        if verb not in flags:
            continue  # an unshipped verb is test_skill_names_every_shipped_verb_and_no_other's to report
        for flag in FLAG.findall(rest):
            assert flag in flags[verb], f"gc follow-through {verb} takes no {flag}"
    every = set().union(*flags.values())
    assert set(FLAG.findall(skill_text())) <= every, sorted(set(FLAG.findall(skill_text())) - every)


def test_the_sentences_the_founders_rule_on_are_there():
    prose = " ".join(skill_text().split())
    assert NO_SECOND_FOLLOW_UP in prose
    assert MAJOR_FEATURE in prose


def test_skill_has_no_em_dash():
    assert "—" not in skill_text()

"""The follow-through skill against the pack it teaches.

The skill is the mayor's only guide to when it may write to an owner, so it
must name the verbs the pack ships and no others, and it must keep the two
rules the founders rule on word for word.
"""
import os
from pathlib import Path
import re
import shlex
import shutil
import subprocess
import tomllib

import yaml

PACK = Path(__file__).resolve().parents[1]
SKILL = PACK / "skills/follow-through/SKILL.md"

# A change to either sentence is a founder's decision, not an edit.
NO_SECOND_FOLLOW_UP = "There is no second follow-up, ever."
MAJOR_FEATURE = "A note about a new ability goes out only when you were given one for a major feature."

# A command runs from "gc follow-through" to the end of its code span or line.
COMMAND = re.compile(r"gc\s+follow-through\s+([^\s`]+)([^`\n]*)")
FLAG = re.compile(r"--[a-z][a-z-]*")


def skill_text() -> str:
    return SKILL.read_text(encoding="utf-8")


def commands(text: str) -> list[tuple[str, list[str]]]:
    """Each gc follow-through command in text: its whole verb word, and the
    options it passes, split the way a shell splits them, so a quoted value
    is one argument and never an option."""
    return [(verb, [arg.split("=", 1)[0] for arg in shlex.split(rest) if arg.startswith("--")])
            for verb, rest in COMMAND.findall(text)]


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
    fields = yaml.safe_load(match.group("body"))
    assert fields["name"] == "follow-through"
    description = fields["description"]
    assert description.strip() and "\n" not in description, "the description is one line"


def test_the_command_reader_takes_whole_words_and_quoted_values():
    assert commands("`gc follow-through add_item --summary x`") == [("add_item", ["--summary"])]
    assert commands("gc follow-through status2") == [("status2", [])]
    assert commands("run `gc follow-through  reopen ID` now") == [("reopen", [])]
    assert commands('gc follow-through remind --in 2h --text "Run git status --short"') == [("remind", ["--in", "--text"])]
    assert commands('`gc follow-through hours C --hours "Mon-Fri 09:00-18:00" --tz=Asia/Tokyo`') == [("hours", ["--hours", "--tz"])]


def test_skill_names_every_shipped_verb_and_no_other():
    named = {verb for verb, _ in commands(skill_text())}
    shipped = set(shipped_verbs())
    assert not shipped - named, f"verbs the pack ships that the skill never names: {sorted(shipped - named)}"
    assert not named - shipped, f"verbs the skill names that the pack does not ship: {sorted(named - shipped)}"


def test_verbs_list_gives_each_verb_its_help_line():
    section = skill_text().split("\n## The verbs\n", 1)
    assert len(section) == 2, "the skill has no section headed The verbs"
    listed = sorted(line for line in section[1].splitlines() if line.startswith("- "))
    assert listed == sorted(f"- `gc follow-through {verb}`: {description}"
                            for verb, description in shipped_verbs().items())


def test_every_flag_in_the_skill_is_one_its_verb_takes():
    flags = verb_flags()
    for verb, options in commands(skill_text()):
        if verb not in flags:
            continue  # an unshipped verb is test_skill_names_every_shipped_verb_and_no_other's to report
        for flag in options:
            assert flag in flags[verb], f"gc follow-through {verb} takes no {flag}"
    # A flag named on its own in the prose, as `--off`, belongs to some verb.
    alone = set(re.findall(r"`(--[a-z][a-z-]*)`", skill_text()))
    every = set().union(*flags.values())
    assert alone <= every, sorted(alone - every)


def test_the_sentences_the_founders_rule_on_are_there():
    prose = " ".join(skill_text().split())
    assert NO_SECOND_FOLLOW_UP in prose
    assert MAJOR_FEATURE in prose


def test_skill_has_no_em_dash():
    assert "—" not in skill_text()

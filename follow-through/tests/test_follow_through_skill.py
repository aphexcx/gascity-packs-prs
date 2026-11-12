"""The follow-through skill against the pack it teaches.

The skill is the mayor's only guide to when it may write to an owner, so it
must name the verbs the pack ships and no others, it must have a rule for
every mail the pack sends, and it must keep each rule the founders made word
for word, in its own section.
"""
import os
from pathlib import Path
import re
import shlex
import shutil
import subprocess
import tomllib
from typing import NamedTuple

import pytest
import yaml

PACK = Path(__file__).resolve().parents[1]
SKILL = PACK / "skills/follow-through/SKILL.md"
SOURCES = sorted((PACK / "src").glob("*.js"))
CLI = PACK / "src/cli.js"


class Rule(NamedTuple):
    name: str
    source: str
    section: str
    sentence: str


# Each rule the founders made that the skill carries, with where it came from,
# the section it stands in and its sentence word for word. A change to any of
# these sentences is a founder's decision, not an edit. A new founder's rule
# adds an entry.
FOUNDERS_RULES = [
    Rule("never for work you owe", "contract item 2", "When to open an item",
         "Never open one for work you owe."),
    Rule("never for small talk", "contract item 2", "When to open an item",
         "Never open one for small talk."),
    Rule("unsure means out", "contract item 3", "The one follow-up",
         "If you are unsure, it stays out."),
    Rule("none in, nothing sent", "contract item 3", "The one follow-up",
         "When no item goes in, send nothing."),
    Rule("one follow-up", "contract item 3", "The one follow-up",
         "There is no second follow-up, ever."),
    Rule("no blame", "contract item 4", "What a follow-up sounds like",
         'Write nothing that blames, such as "you forgot" or "still waiting on you".'),
    Rule('no "just checking in"', "contract item 4", "What a follow-up sounds like",
         'Never write "just checking in".'),
    Rule("nothing added", "contract item 4", "What a follow-up sounds like",
         "Add no offer, greeting or news to it."),
    Rule("only on the pack's mail", "contract item 5", "When you may not write",
         "Write a follow-up only because a follow-through mail asked for it."),
    Rule("never with consent off", "contract item 5", "When you may not write",
         "Never write a follow-up to an owner whose consent is off."),
    Rule("the daytime bound", "contract item 5", "When you may not write",
         "Write one only between 08:00 and 20:00 in the owner's time, and inside the hours they gave you."),
    Rule("outside the hours, nothing", "contract item 5", "When you may not write",
         "If it is now outside those hours, send nothing for those items."),
    Rule("unrecorded consent is off", "contract item 6", "Consent, hours and zone",
         "Consent that was never recorded counts as off."),
    Rule("ask once, in their conversation", "contract item 6", "Consent, hours and zone",
         "Ask the owner once, in a conversation they started, whether they want you to come back to things they leave open."),
    Rule("stop at once", "contract item 6", "Consent, hours and zone",
         "When the owner says stop, record off at once for every conversation you have with them, before anything else in that turn."),
    Rule("the zone never guessed", "contract item 6", "Consent, hours and zone",
         "Never guess it, and never use the box's own."),
    Rule("a reminder never only in the session", "contract item 7", "Reminders",
         "Never keep a reminder only in your own session."),
    Rule("told set only once stored", "contract item 7", "Reminders",
         "Only then tell the owner it is set: react with ⏰ where the channel lets you react, and otherwise answer with one short line that says when."),
    Rule("cancelled only on the verb's word", "contract item 7", "Reminders",
         "Tell the owner it is cancelled only when the verb says so."),
    Rule("no free text after the window", "contract item 8", "WhatsApp",
         "A follow-up or a reminder that comes due after that window has closed is not sent there as free text."),
    Rule("never the update template", "contract item 8, Afik's WhatsApp rule", "WhatsApp",
         "It is never sent as the update template either: by Afik's rule that template is only for an actual update, and a follow-up or a reminder is not one."),
    Rule("no unasked news", "contract item 9", "News about yourself",
         "Never write to the owner, unasked, about an update to yourself."),
    Rule("a major feature only", "contract item 9, Afik's word on release notes", "News about yourself",
         "A note about a new ability goes out only when you were given one for a major feature."),
    Rule("no message on the no-daytime-hours mail", "Afik, 2026-11-12", "No daytime hours",
         "Never write to the owner because of this mail: wait until they next write to you."),
]

# A command runs from "gc follow-through" to the end of its code span or line.
COMMAND = re.compile(r"gc\s+follow-through\s+([^\s`]+)([^`\n]*)")
FLAG = re.compile(r"--[a-z][a-z-]*")

# Each mail the pack sends is a raise, made with a subject written as a plain
# string in the pack's source.
SUBJECT_FIELD = re.compile(r"\bsubject\s*[:=](?!=)")
SUBJECT = re.compile(r"\bsubject\s*[:=](?!=)\s*'([^'\n]+)'")
MAKE_RAISE = re.compile(r"(?<!function )\bmakeRaise\(")
# Each raise is made with its kind, the word status prints for it, as the
# call's second argument.
KIND = re.compile(r"(?<!function )\bmakeRaise\(\s*[\w.]+\s*,\s*'([^'\n]+)'")
# Status counts the mails whose delivery stopped on a line of its own: the
# words before the colon, then the count of raises in the state stopped.
STOPPED_LINE = re.compile(r"`([^`$:\n]+): \$\{[^`}]*'stopped'")
# A mail can arrive twice. The skill's rule for that names the second arrival
# and ends in sending nothing, as the follow-up section's rule does.
ARRIVING_TWICE = re.compile(r"the same [a-z]+ arriving a second time\b[^.]*\.\s+Send nothing\b")


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


def mail_subjects() -> list[str]:
    """The subject of every kind of mail the pack sends."""
    return sorted({subject for path in SOURCES for subject in SUBJECT.findall(path.read_text(encoding="utf-8"))})


def mail_kinds() -> list[str]:
    """The kind of every mail the pack makes, as status prints it."""
    return sorted({kind for path in SOURCES for kind in KIND.findall(path.read_text(encoding="utf-8"))})


def stopped_line_words() -> list[str]:
    """The words status prints before the count of mails whose delivery stopped."""
    return STOPPED_LINE.findall(CLI.read_text(encoding="utf-8"))


def sections(text: str) -> list[str]:
    """Each section of the skill under a ## heading, with its lines joined."""
    return [" ".join(part.split()) for part in re.split(r"^## ", text, flags=re.MULTILINE)[1:]]


def section_bodies(text: str) -> dict[str, str]:
    """Each section of the skill by its ## heading, with its lines joined."""
    parts = re.split(r"^## (.+)$", text, flags=re.MULTILINE)
    return {heading.strip(): " ".join(body.split()) for heading, body in zip(parts[1::2], parts[2::2])}


def opening(text: str) -> str:
    """The skill's text after its front matter and before its first section,
    with its lines joined."""
    body = re.sub(r"\A---\n.*?\n---\n", "", text, count=1, flags=re.DOTALL)
    return " ".join(body.split("\n## ", 1)[0].split())


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


@pytest.mark.parametrize("rule", FOUNDERS_RULES, ids=[rule.name for rule in FOUNDERS_RULES])
def test_each_founders_rule_stands_word_for_word_in_its_section(rule):
    """A founder's rule removed, reworded or moved to another section, or its
    section removed, fails here under the rule's name."""
    body = section_bodies(skill_text()).get(rule.section)
    assert body is not None, f'{rule.name} ({rule.source}): the skill has no section headed "{rule.section}"'
    assert " ".join(rule.sentence.split()) in body, \
        f'{rule.name} ({rule.source}): the section "{rule.section}" does not hold, word for word: {rule.sentence}'


def test_every_mail_subject_in_the_pack_is_read():
    source = "\n".join(path.read_text(encoding="utf-8") for path in SOURCES)
    found = len(SUBJECT.findall(source))
    assert found, "the pack's source names no mail subject"
    assert found == len(SUBJECT_FIELD.findall(source)), "a mail subject in the pack's source is not a plain quoted string"
    assert found == len(MAKE_RAISE.findall(source)), "a raise is made without a subject this test can read"


def test_every_mail_kind_and_the_stopped_line_are_read():
    source = "\n".join(path.read_text(encoding="utf-8") for path in SOURCES)
    assert KIND.findall(source), "the pack's source makes no raise with a kind"
    assert len(KIND.findall(source)) == len(MAKE_RAISE.findall(source)), "a raise is made without a kind this test can read"
    assert len(stopped_line_words()) == 1, "status prints no line this test can read for mail whose delivery stopped"


@pytest.mark.parametrize("subject", mail_subjects())
def test_the_opening_names_every_mail_the_pack_sends(subject):
    """A mayor comes to this skill when one of the pack's mails reaches it, so
    the opening names the subject of every kind of mail the pack sends."""
    assert f'"{subject}"' in opening(skill_text()), f'the skill\'s opening never names the mail "{subject}"'


@pytest.mark.parametrize("kind", mail_kinds())
def test_the_health_section_says_what_a_stopped_mail_of_each_kind_means(kind):
    """After its last try a mail stops for good, and the pack tells nobody but
    status and doctor. The Health section names the line status prints for it,
    and each kind of mail as status prints it, so a kind added later fails
    here until the section says what its stopped mail means."""
    health = section_bodies(skill_text()).get("Health")
    assert health is not None, "the skill has no section headed Health"
    words = stopped_line_words()
    assert words and words[0] in health, f'the Health section never names the status line "{words[0] if words else "?"}"'
    assert f"`{kind}`" in health, f"the Health section never says what a stopped `{kind}` mail means"


@pytest.mark.parametrize("subject", mail_subjects())
def test_each_mail_the_pack_sends_has_a_rule_for_arriving_twice(subject):
    """A repeat is rare, because the pack looks for a mail in the city's mail
    history before it sends that mail again, and it can still happen: a lookup
    that misses lets a second send through. The section for each kind of mail
    must say that a repeat gets nothing, so an owner is never told the same
    thing twice. A section that names two kinds of mail needs a rule for each."""
    subjects = mail_subjects()
    own = [section for section in sections(skill_text()) if f'"{subject}"' in section]
    assert own, f'no section of the skill names the mail "{subject}"'
    assert any(len(ARRIVING_TWICE.findall(section)) >= sum(f'"{s}"' in section for s in subjects)
               for section in own), \
        f'the skill\'s section for "{subject}" has no rule for the same mail arriving a second time'


def test_skill_has_no_em_dash():
    assert "—" not in skill_text()

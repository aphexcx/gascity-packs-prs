"""Usage: python3 -I breaks.py PACK_DIR OUT_DIR

Breaks the follow-through skill in copies of the pack and runs the skill's
own test on each copy. First the 48 breaks of the founders' rules: each rule's
sentence removed, then each replaced by its reversal. Then the read's breaks
M01 to M16, replayed with the read's own edits. Prints one block per break.
"""
import ast
import re
import shutil
import subprocess
import sys
from pathlib import Path

pack_dir, out = (Path(p).resolve() for p in sys.argv[1:3])
out.mkdir(parents=True, exist_ok=True)
SKILL = "skills/follow-through/SKILL.md"
TEST = "tests/test_follow_through_skill.py"
CASE = "test_each_founders_rule_stands_word_for_word_in_its_section"
have = subprocess.run(["python3", "-c", "import pytest, yaml"], capture_output=True).returncode == 0
RUN = (["python3", "-m", "pytest"] if have else
       ["uv", "run", "--with", "pytest", "--with", "pyyaml", "--with", "jsonschema", "python3", "-m", "pytest"])


def founders_rules() -> list[tuple[str, str]]:
    """(name, sentence) of each entry of FOUNDERS_RULES, read from the test's source."""
    tree = ast.parse((pack_dir / TEST).read_text(encoding="utf-8"))
    for node in tree.body:
        if isinstance(node, ast.Assign) and getattr(node.targets[0], "id", None) == "FOUNDERS_RULES":
            return [(call.args[0].value, call.args[3].value) for call in node.value.elts]
    raise SystemExit("no FOUNDERS_RULES in the test")


# The reversal of each rule, from the round's contract (item 3's table, last column).
REVERSALS = {
    "never for work you owe": "Open one for work you owe, too.",
    "never for small talk": "Open one for small talk when it helps.",
    "unsure means out": "If you are unsure, it goes in.",
    "none in, nothing sent": "When no item goes in, send a short note anyway.",
    "one follow-up": "There may be a second follow-up after a week.",
    "no blame": 'You may write "still waiting on you".',
    'no "just checking in"': 'You may write "just checking in".',
    "nothing added": "Add a greeting to it.",
    "only on the pack's mail": "Write a follow-up whenever you see an open item in the list.",
    "never with consent off": "You may write a follow-up to an owner whose consent is off.",
    "the daytime bound": ("SUB", "20:00", "22:00"),
    "outside the hours, nothing": "If it is now outside those hours, send them anyway.",
    "unrecorded consent is off": "Consent that was never recorded counts as on.",
    "ask once, in their conversation": "Ask the owner every week, in any conversation, whether they want you to come back to things they leave open.",
    "stop at once": "When the owner says stop, record off at the end of the day.",
    "the zone never guessed": "If they do not say, use the box's own.",
    "a reminder never only in the session": "You may keep a reminder only in your own session.",
    "told set only once stored": ("SUB", "Only then tell the owner it is set", "Before you run the verb, tell the owner it is set"),
    "cancelled only on the verb's word": "Tell the owner it is cancelled as soon as they ask.",
    "no free text after the window": "A follow-up or a reminder that comes due after that window has closed is sent there as free text anyway.",
    "never the update template": "It may be sent as the update template instead.",
    "no unasked news": "Tell the owner about each update to yourself.",
    "a major feature only": "A note about a new ability goes out for every update.",
    "no message on the no-daytime-hours mail": "Write to the owner about it as soon as this mail arrives.",
}

# The read's breaks, with the read's own edits (its mutate.py at 39628196).
# Each old text is matched with any whitespace between its words, because
# this round rewrapped some lines.
COVER = '''## A notice about hours

A mail with the subject "follow-through: no daytime hours" says an owner's
hours allow no follow-up. If you already told the owner so, it is the same
notice arriving a second time. Send nothing for it.

'''
READ_BREAKS = [
    ("M01 consent sentence removed",
     [("- Never write a follow-up to an owner whose consent is off. If the owner\n  said stop after the mail was sent, send nothing.\n", "")]),
    ("M02 20:00 changed to 22:00 in both places",
     [("between 08:00 and 20:00 in the owner's time", "between 08:00 and 22:00 in the owner's time"),
      ("only between 08:00 and\n20:00 in their time", "only between 08:00 and\n22:00 in their time")]),
    # The read's edit turned "Never write about those items again." into this
    # sentence. MINOR 9's ruling removed that sentence in this round, so the
    # replay puts the read's words where it stood.
    ("M03 a second follow-up allowed, the pinned sentence kept",
     [("the way that channel's skill says.\nWhen no item goes in",
       "the way that channel's skill says. Write about\nthose items again after a week if the owner has not answered.\nWhen no item goes in")]),
    ("M03b the pinned no-second-follow-up sentence removed",
     [("There is no second follow-up, ever. ", "")]),
    ("M04 the update template allowed after the window",
     [("It is never sent as\nthe update template either: by Afik's rule that template is only for an\nactual update, and a follow-up or a reminder is not one.",
       "Send it as\nthe update template instead.")]),
    ("M05 a flag renamed (--summary to --title)",
     [('--summary "Choose', '--title "Choose')]),
    ("M06 the outside-hours send-nothing sentence removed",
     [(" If it is now outside those hours, send nothing for those\nitems.", "")]),
    ("M07 the whole section When you may not write removed", "SECTION:## When you may not write"),
    ("M08 the whole WhatsApp section removed", "SECTION:## WhatsApp"),
    ("M09 the zone may be guessed from the box",
     [("Never guess\nit, and never use the box's own.", "Guess it\nfrom the box's own when they do not say.")]),
    ("M10 the pinned major-feature sentence removed",
     [("A note about\na new ability goes out only when you were given one for a major feature. ", "")]),
    ("M11 a follow-up may be written from the list, without a mail",
     [("- Write a follow-up only because a follow-through mail asked for it. Never\n  write one because you happen to be awake, or because you saw an open item\n  in the list.\n",
       "- Write a follow-up whenever you see an open item in the list.\n")]),
    ("M12 the reminder's arriving-twice rule removed",
     [("it is the same\nreminder arriving a second time, even if this mail says it is late. Send\nnothing for it, as for a cancelled reminder.", "give it again.")]),
    ("M13 ask for consent every week, in any conversation",
     [("Ask the owner once, in a conversation they started,", "Ask the owner every week, in any conversation,")]),
    ("M14 consent never recorded counts as on",
     [("Consent that was never recorded counts as off.", "Consent that was never recorded counts as on.")]),
    ("M15 the pinned sentence kept and contradicted in the next sentence",
     [("There is no second follow-up, ever. The pack will not ask again about an\nitem it raised.",
       "There is no second follow-up, ever. The exception is an item worth money to\nthe owner, which gets one more.")]),
    # At the read's head the skill had no section for the third mail, so the
    # read's rote section was the only one. This round wrote that section, so
    # the replay puts the rote section in its place.
    ("M16 the rote section for the third mail in place of this round's", "REPLACE:## No daytime hours"),
    # The read's edit as written: the rote section added. At this head it sits
    # beside this round's section and removes nothing.
    ("M16-as-written the rote section added beside this round's",
     [("## News about yourself\n", COVER + "## News about yourself\n")]),
]


def flexible(old: str) -> re.Pattern:
    return re.compile(r"\s+".join(re.escape(word) for word in old.split()))


def replace_once(text: str, old: str, new: str, label: str) -> str:
    pattern = flexible(old)
    found = pattern.findall(text)
    assert len(found) == 1, (label, old, len(found))
    return pattern.sub(lambda _: new, text, count=1)


def section_span(text: str, heading: str) -> tuple[int, int]:
    start = text.index(heading + "\n")
    return start, text.index("\n## ", start + 1) + 1


def run(pack: Path) -> tuple[str, list[str], int]:
    result = subprocess.run(RUN + [TEST, "-q", "-p", "no:cacheprovider", "--tb=no", "-rf"],
                            cwd=pack, capture_output=True, text=True, timeout=600)
    lines = result.stdout.strip().splitlines()
    failed = sorted(line.split("::", 1)[1].split(" - ")[0] for line in lines if line.startswith("FAILED"))
    return (lines[-1] if lines else result.stderr.strip()[-200:]), failed, result.returncode


def fresh(name: str) -> Path:
    pack = out / name / "follow-through"
    if pack.parent.exists():
        shutil.rmtree(pack.parent)
    shutil.copytree(pack_dir, pack)
    return pack


def report(name: str, failed: list[str], summary: str, code: int, verdict: str) -> None:
    print(f"== {name}")
    print("failing: " + ("; ".join(failed) if failed else "none"))
    print(f"counts: {summary}")
    print(f"exit: {code}")
    print(f"verdict: {verdict}\n")


pack = fresh("head")
summary, baseline, code = run(pack)
report("HEAD unchanged", baseline, summary, code, "GREEN" if not baseline and code == 0 else "NOT GREEN")

rules = founders_rules()
assert [name for name, _ in rules] == list(REVERSALS), "the reversals do not match the test's rules, one for one"
tally = {"red": 0, "not": []}
for number, (name, sentence) in enumerate(rules, 1):
    reversal = REVERSALS[name]
    if isinstance(reversal, tuple):
        _, old, new = reversal
        assert old in sentence, (name, old)
        reversal = sentence.replace(old, new)
    for letter, label, replacement in (("a", "sentence removed", ""), ("b", "sentence reversed", reversal)):
        tag = f"B{number:02d}{letter} {name}: {label}"
        pack = fresh(f"B{number:02d}{letter}")
        text = (pack / SKILL).read_text(encoding="utf-8")
        (pack / SKILL).write_text(replace_once(text, sentence, replacement, tag), encoding="utf-8")
        summary, failed, code = run(pack)
        hit = f"{CASE}[{name}]" in failed and code != 0
        tally["red"] += hit
        if not hit:
            tally["not"].append(tag)
        report(tag, failed, summary, code, "RED on its row" if hit else "NOT RED on its row")

for name, edits in READ_BREAKS:
    tag = name.split()[0]
    pack = fresh(tag)
    text = (pack / SKILL).read_text(encoding="utf-8")
    if isinstance(edits, str) and edits.startswith("SECTION:"):
        start, end = section_span(text, edits.split(":", 1)[1])
        text = text[:start] + text[end:]
    elif isinstance(edits, str) and edits.startswith("REPLACE:"):
        start, end = section_span(text, edits.split(":", 1)[1])
        text = text[:start] + COVER + text[end:]
    else:
        for old, new in edits:
            text = replace_once(text, old, new, name)
    (pack / SKILL).write_text(text, encoding="utf-8")
    summary, failed, code = run(pack)
    report(name, failed, summary, code, "RED" if failed and code != 0 else "GREEN")

print(f"founders' rule breaks red on their own row: {tally['red']} of {2 * len(rules)}")
for tag in tally["not"]:
    print(f"not red on its row: {tag}")

"""Usage: python3 -I breaks.py PACK_DIR OUT_DIR

Breaks the follow-through pack in copies and runs the skill's own test on
each copy. B1 to B4 each make one edit to the skill. B5 makes the pack's
status name the reminder a stopped raise carries. Prints one block per
break, with the cases that failed and whether they are the ones expected.
"""
import os
import re
import shutil
import subprocess
import sys
from pathlib import Path

pack_dir, out = (Path(p).resolve() for p in sys.argv[1:3])
out.mkdir(parents=True, exist_ok=True)
SKILL = "skills/follow-through/SKILL.md"
TEST = "tests/test_follow_through_skill.py"
FOUNDERS = "test_each_founders_rule_stands_word_for_word_in_its_section"
HEALTH = "test_the_health_section_says_what_a_stopped_mail_of_each_kind_means"
STOPPED_REMINDER = "test_the_stopped_reminder_rule_takes_every_overdue_reminder_of_its_conversation"
have = subprocess.run(["python3", "-c", "import pytest, yaml"], capture_output=True).returncode == 0
RUN = (["python3", "-m", "pytest"] if have else
       ["uv", "run", "--with", "pytest", "--with", "pyyaml", "--with", "jsonschema", "python3", "-m", "pytest"])
UUID = re.compile(r"[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}")

ENTRY_25 = "For a stopped `follow-up`, send nothing, and never make up for it by writing to the owner yourself."
# Each break: its name, the edits as (file, old, new), and the cases it must
# fail, no more and no fewer. Skill edits match any whitespace between words.
BREAKS = [
    ("B1 entry 25's sentence removed from the Health bullet",
     [(SKILL, ENTRY_25 + " ", "")],
     {f"{FOUNDERS}[no making up for a stopped follow-up]", f"{HEALTH}[follow-up]"}),
    ("B2 entry 25 reversed",
     [(SKILL, ENTRY_25, "For a stopped `follow-up`, make up for it by writing to the owner yourself.")],
     {f"{FOUNDERS}[no making up for a stopped follow-up]"}),
    ("B3 the sentence that status does not say which reminder stopped removed",
     [(SKILL, "Status does not say which reminder stopped. ", "")],
     {STOPPED_REMINDER}),
    ("B4 every reminder of that conversation changed to one",
     [(SKILL, "for every reminder of that conversation", "for one reminder of that conversation")],
     {STOPPED_REMINDER}),
    # status() carries no reminder id, so the stopped line has nothing to
    # print until status() maps it too: two lines, as the pack's own cure
    # would need.
    ("B5 status names the reminder a stopped raise carries (the pack, in a copy)",
     [("src/engine.js", "expires_at: null, error: r.error }))",
       "expires_at: null, error: r.error, reminder_id: r.reminder_id }))"),
      ("src/cli.js", "${r.error || 'unconfirmed send'}`),",
       "${r.error || 'unconfirmed send'}${r.state === 'stopped' && r.reminder_id ? `; reminder ${r.reminder_id}` : ''}`),")],
     {STOPPED_REMINDER}),
]


def flexible(old: str) -> re.Pattern:
    return re.compile(r"\s+".join(re.escape(word) for word in old.split()))


def edit(pack: Path, name: str, old: str, new: str, label: str) -> None:
    path = pack / name
    text = path.read_text(encoding="utf-8")
    if name == SKILL:
        pattern = flexible(old)
        assert len(pattern.findall(text)) == 1, (label, old)
        text = pattern.sub(lambda _: new, text, count=1)
    else:
        assert text.count(old) == 1, (label, old)
        text = text.replace(old, new)
    path.write_text(text, encoding="utf-8")


def run(pack: Path) -> tuple[str, list[str], list[str], int]:
    # A wide terminal, so pytest keeps each failure's message on its summary line.
    result = subprocess.run(RUN + [TEST, "-q", "-p", "no:cacheprovider", "--tb=no", "-rf"],
                            cwd=pack, capture_output=True, text=True, timeout=600,
                            env={**os.environ, "COLUMNS": "1000"})
    lines = result.stdout.strip().splitlines()
    rows = [line.split("::", 1)[1] for line in lines if line.startswith("FAILED")]
    failed = sorted(row.split(" - ")[0] for row in rows)
    messages = sorted(UUID.sub("<uuid>", row.split(" - ", 1)[1]) for row in rows if " - " in row)
    return (lines[-1] if lines else result.stderr.strip()[-200:]), failed, messages, result.returncode


def fresh(name: str) -> Path:
    pack = out / name / "follow-through"
    if pack.parent.exists():
        shutil.rmtree(pack.parent)
    shutil.copytree(pack_dir, pack, ignore=shutil.ignore_patterns("__pycache__", ".pytest_cache"))
    return pack


def report(name: str, failed: list[str], messages: list[str], summary: str, code: int, verdict: str) -> None:
    print(f"== {name}")
    print("failing: " + ("; ".join(failed) if failed else "none"))
    for message in messages:
        print(f"message: {message}")
    print(f"counts: {summary}")
    print(f"exit: {code}")
    print(f"verdict: {verdict}\n")


pack = fresh("head")
summary, failed, messages, code = run(pack)
report("HEAD unchanged", failed, messages, summary, code, "GREEN" if not failed and code == 0 else "NOT GREEN")

met = 0
for name, edits, expected in BREAKS:
    tag = name.split()[0]
    pack = fresh(tag)
    for path, old, new in edits:
        edit(pack, path, old, new, name)
    summary, failed, messages, code = run(pack)
    ok = set(failed) == expected and code != 0
    met += ok
    report(name, failed, messages, summary, code,
           "RED on the expected cases and no other" if ok else f"NOT AS EXPECTED; expected: {sorted(expected)}")

print(f"breaks red on exactly their expected cases: {met} of {len(BREAKS)}")

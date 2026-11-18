"""Keep the literal make commands in the verification skill runnable."""

import re
import shlex
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]


def test_verify_skill_make_targets_exist():
    skill = (ROOT / ".claude/skills/verify/SKILL.md").read_text()
    makefile = (ROOT / "Makefile").read_text()
    rules = set()
    for match in re.finditer(r"^([\w./%-]+(?: +[\w./%-]+)*) *:(?!=)", makefile, re.M):
        rules.update(match[1].split())

    targets = set()
    for block in re.findall(r"^```[^\n]*\n(.*?)^```\s*$", skill, re.M | re.S):
        for line in block.replace("\\\n", " ").splitlines():
            shell = shlex.shlex(line, posix=True, punctuation_chars=";&|()")
            shell.whitespace_split = True
            reading_targets = False
            for token in shell:
                if token in {";", "&&", "||", "|", "(", ")"}:
                    reading_targets = False
                elif token == "make":
                    reading_targets = True
                elif reading_targets and not token.startswith("-") and "=" not in token:
                    targets.add(token)

    assert targets, "No make targets found in the verification skill"
    missing = sorted(targets - rules)
    assert not missing, "Undefined Makefile targets: " + ", ".join(missing)

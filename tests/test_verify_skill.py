"""Check the documented commands, repository paths, and credential boundary."""

import os
import re
import shlex
import subprocess
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
OPERATORS = {";", "&&", "||", "|", "&", "(", ")", "<", ">"}


def shell_words(line):
    # Keep quotes until after recognizing control operators. A quoted '#' or
    # '&&' is data, even when it is a complete argument on its own.
    lexer = shlex.shlex(line, posix=False, punctuation_chars=True)
    lexer.whitespace_split = True
    return list(lexer)


def check_words(words, rules, missing):
    reading_targets = False
    for index, raw in enumerate(words):
        word = raw[1:-1] if raw.startswith(("'", '"')) and raw[-1] == raw[0] else raw
        if raw in OPERATORS:
            reading_targets = False
        elif word == "make":
            reading_targets = True
        elif reading_targets and not word.startswith("-") and "=" not in word:
            if word not in rules:
                missing.add(word)

        if word in {"sh", "bash"} and index + 2 < len(words):
            if re.fullmatch(r"-[a-zA-Z]*c[a-zA-Z]*", words[index + 1]):
                script = shlex.split(words[index + 2])[0]
                check_words(shell_words(script), rules, missing)
        if raw == "-C":
            directory = shlex.split(words[index + 1])[0]
            if not (ROOT / directory).is_dir():
                missing.add(directory)
        if re.fullmatch(r"[\w.*?/-]+\.(?:py|sh|js|cjs|mjs|toml)", word):
            if not any(ROOT.glob(word)):
                missing.add(word)


def test_verify_skill_make_targets_exist(tmp_path):
    skill_file = ROOT / ".claude/skills/verify/SKILL.md"
    skill = skill_file.read_text()
    makefile = (ROOT / "Makefile").read_text()
    rules = set()
    for match in re.finditer(r"^([\w./%-]+(?: +[\w./%-]+)*) *:(?!=)", makefile, re.M):
        rules.update(match[1].split())

    fences = re.findall(r"^```([^\n]*)\n(.*?)^```[ \t]*$", skill, re.M | re.S)
    setup, *commands = fences
    assert setup[0] == "bash" and setup[1].startswith("verify()")
    assert commands, "Missing verification commands"
    missing = set()
    for kind, block in commands:
        assert kind == "sh"
        for line in block.strip().splitlines():
            words = shell_words(line)
            assert words and words[0] == "verify", f"Missing credential guard: {line}"
            assert OPERATORS.isdisjoint(words), f"Unguarded shell operation: {line}"
            check_words(words, rules, missing)
    for path in re.findall(r"`([\w.-]+/[\w./-]*|[\w.-]+\.toml)`", skill):
        if not (ROOT / path).exists():
            missing.add(path)
    for link in re.findall(r"\]\(([^)]+)\)", skill):
        if "://" not in link and not (skill_file.parent / link).is_file():
            missing.add(link)
    assert not missing, "Undefined Makefile targets or missing paths: " + ", ".join(sorted(missing))

    # Run the published function with inert sentinels inside an empty Git fixture.
    subprocess.run(["git", "init", "-q", str(tmp_path)], check=True)
    env = {**os.environ, "VERIFY_TEST_SECRET": "sentinel"}
    probe = setup[1] + "\nverify sh -ec 'test -z \"${VERIFY_TEST_SECRET+x}\" && touch started'"

    def run_probe():
        return subprocess.run(["bash", "-c", probe], cwd=tmp_path, env=env, capture_output=True)

    started = tmp_path / "started"
    result = run_probe()
    assert result.returncode == 0 and started.exists(), result.stderr.decode()
    started.unlink()
    (tmp_path / ".gitignore").write_text(".env.local\n")
    (tmp_path / ".env.local").write_text("VERIFY_TEST_SECRET=sentinel\n")
    assert run_probe().returncode != 0 and not started.exists()

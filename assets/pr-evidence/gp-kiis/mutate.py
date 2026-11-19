"""Mutation check for THE REVIEW RULES tests: apply each text mutation to the
fragment (or README) in a scratch clone, run the fragment's static tests, and
record whether they went red. Usage:
  python3 -I mutate.py <scratch-clone> <mutations.json> <out.txt>
Each mutation is {"name", "file", "old", "new"}; "file" must resolve to a file
inside the scratch clone, and "old" must occur in it exactly once (matched with
the file's line ends read as newlines, and written back with the file's own).
The clone must be green before the first mutation. Each run reads pytest's
JUnit XML report, never its console text, with PYTEST_ADDOPTS left out of its
environment; a mutation counts as KILLED only when pytest exits 1 and the
report names a failing fragment test, and a run that exits otherwise, or runs
fewer tests than the green baseline, is BROKEN, never a kill. The original
bytes are written back after every run (never a git command). The out file
is cleared first, so it never holds an earlier run's report; exit 0 only when
there was at least one mutation and every mutation was killed."""

from __future__ import annotations

import json
import os
import re
import subprocess
import sys
import tempfile
import xml.etree.ElementTree as ET
from pathlib import Path

TESTS = "tests/test_gc_standing_notes_fragment.py"
MODULE = re.compile(r"(?:^|\.)test_gc_standing_notes_fragment(?:\.|$)")
clone = Path(sys.argv[1]).resolve()
out = Path(sys.argv[3])
out.write_text("the run did not finish\n", encoding="utf-8")
mutations = json.loads(Path(sys.argv[2]).read_text(encoding="utf-8"))
env = {key: value for key, value in os.environ.items() if key != "PYTEST_ADDOPTS"}


def run_tests() -> tuple[int, int, list[str]]:
    """pytest's exit code, the fragment tests it ran, and those that failed."""
    with tempfile.TemporaryDirectory() as tmp:
        report = Path(tmp) / "report.xml"
        run = subprocess.run(
            [sys.executable, "-m", "pytest", TESTS, "-q", "-p", "no:cacheprovider",
             f"--junit-xml={report}"],
            cwd=clone, env=env, capture_output=True, text=True,
        )
        cases = list(ET.parse(report).getroot().iter("testcase")) if report.is_file() else []
    cases = [case for case in cases if MODULE.search(case.get("classname", ""))]
    failed = [case.get("name", "") for case in cases
              if case.find("failure") is not None or case.find("error") is not None]
    return run.returncode, len(cases), failed


code, baseline, failed = run_tests()
if code != 0 or failed or not baseline:
    message = f"the clone is not green before any mutation (exit {code}, {baseline} tests ran); nothing was run"
    out.write_text(message + "\n", encoding="utf-8")
    sys.exit(message)
out_lines = []
verdicts = {"KILLED": 0, "SURVIVED": 0, "BROKEN": 0, "SETUP-ERROR": 0}
for m in mutations:
    path = (clone / m["file"]).resolve()
    if not path.is_relative_to(clone) or not path.is_file():
        verdicts["SETUP-ERROR"] += 1
        out_lines.append(f"SETUP-ERROR {m['name']}: {m['file']!r} is not a file inside the clone")
        continue
    original = path.read_bytes()
    eol = "\r\n" if b"\r\n" in original else "\n"
    text = original.decode("utf-8").replace("\r\n", "\n")
    count = text.count(m["old"])
    if count != 1:
        verdicts["SETUP-ERROR"] += 1
        out_lines.append(f"SETUP-ERROR {m['name']}: old text occurs {count} times")
        continue
    try:
        path.write_bytes(text.replace(m["old"], m["new"], 1).replace("\n", eol).encode("utf-8"))
        code, ran, failed = run_tests()
    finally:
        path.write_bytes(original)
    if code == 1 and failed and ran == baseline:
        verdict = "KILLED"
    elif code == 0 and ran == baseline:
        verdict = "SURVIVED"
    else:
        verdict = "BROKEN"
    verdicts[verdict] += 1
    out_lines.append(f"{verdict} {m['name']} (exit {code}; {ran} of {baseline} ran; "
                     f"red: {', '.join(failed) or 'none'})")
out_lines.append(f"{len(mutations)} mutations: " + ", ".join(f"{n} {v.lower()}" for v, n in verdicts.items()))
out.write_text("\n".join(out_lines) + "\n", encoding="utf-8")
print("\n".join(out_lines))
sys.exit(0 if mutations and verdicts["KILLED"] == len(mutations) else 1)

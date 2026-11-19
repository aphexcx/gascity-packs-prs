"""Mutation check for THE REVIEW RULES tests: apply each text mutation to the
fragment (or README) in a scratch clone, run the fragment's static tests, and
record whether they went red. Usage:
  python3 -I mutate.py <scratch-clone> <mutations.json> <out.txt>
Each mutation is {"name", "file", "old", "new"}; "file" must resolve to a file
inside the scratch clone, and "old" must occur in it exactly once (matched with
the file's line ends read as newlines). The clone must be green before the
first mutation. Results come from pytest's JUnit XML report, never its console
text, so options such as PYTEST_ADDOPTS cannot change them. A mutation counts
as KILLED only when pytest exits 1 and the report names a failing fragment
test; any other non-zero exit is BROKEN, never a kill. The original bytes are
written back after every run (never a git command). The out file always
describes this run; exit 0 only when every mutation was killed."""

from __future__ import annotations

import json
import subprocess
import sys
import tempfile
import xml.etree.ElementTree as ET
from pathlib import Path

TESTS = "tests/test_gc_standing_notes_fragment.py"
clone = Path(sys.argv[1]).resolve()
mutations = json.loads(Path(sys.argv[2]).read_text(encoding="utf-8"))
out = Path(sys.argv[3])


def run_tests() -> tuple[int, int, list[str]]:
    """pytest's exit code, the fragment tests it ran, and those that failed."""
    with tempfile.TemporaryDirectory() as tmp:
        report = Path(tmp) / "report.xml"
        run = subprocess.run(
            [sys.executable, "-m", "pytest", TESTS, "-q", "-p", "no:cacheprovider",
             f"--junit-xml={report}"],
            cwd=clone, capture_output=True, text=True,
        )
        cases = ET.parse(report).getroot().iter("testcase") if report.is_file() else []
        cases = [case for case in cases
                 if case.get("classname", "").endswith("test_gc_standing_notes_fragment")]
    failed = [case.get("name", "") for case in cases
              if case.find("failure") is not None or case.find("error") is not None]
    return run.returncode, len(cases), failed


code, ran, failed = run_tests()
if code != 0 or failed or not ran:
    message = f"the clone is not green before any mutation (exit {code}, {ran} tests ran); nothing was run"
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
    text = original.decode("utf-8").replace("\r\n", "\n")
    count = text.count(m["old"])
    if count != 1:
        verdicts["SETUP-ERROR"] += 1
        out_lines.append(f"SETUP-ERROR {m['name']}: old text occurs {count} times")
        continue
    try:
        path.write_bytes(text.replace(m["old"], m["new"], 1).encode("utf-8"))
        code, ran, failed = run_tests()
    finally:
        path.write_bytes(original)
    verdict = "KILLED" if code == 1 and failed else "SURVIVED" if code == 0 and ran else "BROKEN"
    verdicts[verdict] += 1
    out_lines.append(f"{verdict} {m['name']} (exit {code}; red: {', '.join(failed) or 'none'})")
out_lines.append(f"{len(mutations)} mutations: " + ", ".join(f"{n} {v.lower()}" for v, n in verdicts.items()))
out.write_text("\n".join(out_lines) + "\n", encoding="utf-8")
print("\n".join(out_lines))
sys.exit(0 if verdicts["KILLED"] == len(mutations) else 1)

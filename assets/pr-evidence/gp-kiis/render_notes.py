"""Render a city lane that opts into the standing notes with real gc and
print the notes it gets, with the counts and order the ticket's acceptance
asks for. Usage: python3 render_notes.py <repo-root> <gc-binary>.

Uses the integration test module's own helpers, so the lane is the same
city-side lane the GC_TEST_BIN tests render."""

from __future__ import annotations

import importlib.util
import re
import shutil
import sys
import tempfile
from pathlib import Path

repo = Path(sys.argv[1]).resolve()
# Resolved here, before any subprocess runs in the fixture city: a path
# relative to the caller (./build/gc) or a name on PATH (gc) still finds it.
gc_bin = Path(shutil.which(sys.argv[2]) or sys.argv[2]).expanduser().resolve()
spec = importlib.util.spec_from_file_location(
    "integration", repo / "tests" / "test_gc_role_prompt_integration.py"
)
mod = importlib.util.module_from_spec(spec)
sys.modules["integration"] = mod
spec.loader.exec_module(mod)

with tempfile.TemporaryDirectory() as tmp:
    root = Path(tmp)
    role_only = mod.render_city_lane(
        gc_bin, root / "role-only", gascity_pack=repo / "gascity", prompt=f"{mod.ROLE_CALL}\n"
    )
    both = mod.render_city_lane(
        gc_bin, root / "both", gascity_pack=repo / "gascity",
        prompt=f"{mod.ROLE_CALL}\n\n{mod.STANDING_NOTES_CALL}\n",
    )

for name, result in (("role-only", role_only), ("with the call line", both)):
    print(f"gc prime --strict lane ({name}): exit {result.returncode}, stderr {len(result.stderr)} bytes")
assert both.stdout.startswith(role_only.stdout)
notes = both.stdout[len(role_only.stdout):]
markers = ("THE DEEPENING NOTE", "THE PR BODY LINES", "THE REVIEW RULES")
for marker in markers:
    print(f"count {marker!r}: {notes.count(marker)}")
positions = [notes.index(marker) for marker in markers]
print(f"in order: {positions == sorted(positions)}")
print(f"byte-equal to role text + newline + fragment body + newline: "
      f"{both.stdout == role_only.stdout + chr(10) + mod.standing_notes_text() + chr(10)}")
for pattern, what in (
    (r"/", "a slash"),
    (r"\b20\d\d-\d\d-\d\d\b", "a date"),
    (mod.BEAD_ID.pattern, "a bead id"),
):
    print(f"rendered notes carry {what}: {re.search(pattern, notes) is not None}")
review = notes[notes.index("## Review rules"):]
print(f"digits in the rendered review rules: {re.findall(r'[0-9]+', review)}")
print()
print("---- the rendered review rules note, as gc prints it ----")
print(review, end="")

"""Build a read-only Go source overlay; run from the repository root."""

import argparse
import json
from pathlib import Path
import re
import subprocess

parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument("base")
parser.add_argument("output", type=Path)
parser.add_argument("--reader", action="store_true")
args = parser.parse_args()
root = Path.cwd().resolve()
output = args.output.resolve()
if output.is_relative_to(root):
    parser.error("output must be outside the repository")
output.mkdir(parents=True, exist_ok=True)
keep = {"history_bound_test.go", "history_upgrade_test.go", "history_exchange_test.go"}
if args.reader:
    keep = {"history_exchange_test.go"}
extended = {
    "gchat/adapter/internal/ledger/history_floor_test.go",
    "gchat/adapter/internal/catchup/history_floor_test.go",
}
mapping = {}
paths = subprocess.check_output(
    ["rg", "--files", "gchat/adapter", "-g", "*.go"], text=True
).splitlines()
for path in paths:
    source = Path(path)
    if source.name in keep or (not args.reader and path in extended):
        continue
    original = subprocess.run(
        ["git", "show", args.base + ":" + path], capture_output=True, text=True
    )
    data = original.stdout
    if original.returncode:
        data = re.search(r"^package \w+", source.read_text(), re.M).group() + "\n"
    target = output / path.replace("/", "_")
    target.write_text(data)
    mapping[str(root / source)] = str(target)
(output / "overlay.json").write_text(json.dumps({"Replace": mapping}) + "\n")
print(output / "overlay.json")

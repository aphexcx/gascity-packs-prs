"""Scratch copy of the adapter with src/ as committed at 864d2f0a (before the
self-review r1 fixes) and the current tests and fakes: the review
regressions run RED against it."""
import pathlib
import shutil
import subprocess
import sys

repo = pathlib.Path(sys.argv[1])
dest = pathlib.Path(sys.argv[2])
commit = sys.argv[3]
if dest.exists():
    shutil.rmtree(dest)
shutil.copytree(repo / "whatsapp" / "adapter", dest)
names = subprocess.run(["git", "-C", str(repo), "ls-tree", "--name-only", f"{commit}:whatsapp/adapter/src"],
                       check=True, capture_output=True, text=True).stdout.split()
for name in names:
    blob = subprocess.run(["git", "-C", str(repo), "show", f"{commit}:whatsapp/adapter/src/{name}"],
                          check=True, capture_output=True).stdout
    (dest / "src" / name).write_bytes(blob)
print("pre-review src at", commit, "->", dest, sorted(names))

"""Scratch copies with the source as committed at 185a6098 (before the keeper
review r2 fixes) and the current tests and fakes, so the r2 regressions run
RED against it:

  <dest>/adapter   the adapter: test/ as now, src/ every file from the commit
  <dest>/whatsapp  the pack: tests/ as now; doctor/check.mjs, commands/*.sh
                   and adapter/src from the commit (the doctor row and verbs)

Usage: python3 -I make_red_keeper_r2.py <worktree> <dest> <commit>"""
import pathlib
import shutil
import subprocess
import sys

repo = pathlib.Path(sys.argv[1])
dest = pathlib.Path(sys.argv[2])
commit = sys.argv[3]


def show(rel):
    return subprocess.run(["git", "-C", str(repo), "show", f"{commit}:{rel}"], check=True, capture_output=True).stdout


def names(rel):
    out = subprocess.run(["git", "-C", str(repo), "ls-tree", "--name-only", f"{commit}:{rel}"],
                         check=True, capture_output=True, text=True).stdout
    return out.split()


if dest.exists():
    shutil.rmtree(dest)
dest.mkdir(parents=True)

src_names = names("whatsapp/adapter/src")
shutil.copytree(repo / "whatsapp" / "adapter", dest / "adapter")
shutil.copytree(repo / "whatsapp", dest / "whatsapp", ignore=shutil.ignore_patterns("__pycache__"))
for root in (dest / "adapter", dest / "whatsapp" / "adapter"):
    for name in src_names:
        (root / "src" / name).write_bytes(show(f"whatsapp/adapter/src/{name}"))
(dest / "whatsapp" / "doctor" / "check.mjs").write_bytes(show("whatsapp/doctor/check.mjs"))
verbs = [n for n in names("whatsapp/commands") if n.endswith(".sh")]
for name in verbs:
    target = dest / "whatsapp" / "commands" / name
    target.write_bytes(show(f"whatsapp/commands/{name}"))
    target.chmod(0o755)
print("pre-r2 src at", commit, "->", dest / "adapter", sorted(src_names))
print("pre-r2 doctor/check.mjs and verbs", sorted(verbs), "->", dest / "whatsapp")

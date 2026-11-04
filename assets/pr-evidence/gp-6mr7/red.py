"""RED for verbs.py: mirror whatsapp/ into a scratch root, mutate one quoted
verb form and one keeper quote in the skill, and expect verbs.py to fail on
exactly those two. Usage: python3 -I red.py <worktree> <scratch-root>"""

import pathlib
import shutil
import subprocess
import sys

src, dst = pathlib.Path(sys.argv[1]), pathlib.Path(sys.argv[2])
if dst.exists():
    shutil.rmtree(dst)
shutil.copytree(src / "whatsapp", dst / "whatsapp", ignore=shutil.ignore_patterns("node_modules", "test", "tests", "gateway"))
skill = dst / "whatsapp/skills/whatsapp/SKILL.md"
text = skill.read_text(encoding="utf-8")
mutations = [
    ("gc whatsapp inbound clear-gap <wa_id>", "gc whatsapp inbound clear-gap <conversation>"),
    ("nobody from the business has written since <time>", "nobody from the owner has written since <time>"),
]
for old, new in mutations:
    assert old in text, old
    text = text.replace(old, new)
skill.write_text(text, encoding="utf-8")
r = subprocess.run([sys.executable, "-I", str(pathlib.Path(__file__).with_name("verbs.py")), str(dst)], capture_output=True, text=True)
fails = [l for l in r.stdout.splitlines() if l.startswith("FAIL")]
print(f"verbs.py rc={r.returncode} on the mutated mirror; {len(fails)} FAIL lines:")
for l in fails:
    print("  " + l)
ok = r.returncode == 1 and any("clear-gap <conversation>" in l for l in fails) and any("nudge (unshifted)" in l for l in fails)
print("RED OK" if ok else "RED NOT SEEN")
sys.exit(0 if ok else 1)

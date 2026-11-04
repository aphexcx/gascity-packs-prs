"""RED for verbs.py: mirror whatsapp/ into a scratch root, mutate one quoted
verb form, one keeper quote and the failed-delivery line in the skill, put a
made-up Meta id on the setup page and four other id or 555 forms in files of
their own, and expect verbs.py to fail on exactly those eight.
Usage: python3 -I red.py <worktree> <scratch-root>"""

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
    ("[delivery failed: <code> <title>]", "[delivery error: <code> <title>]"),
]
for old, new in mutations:
    assert old in text, old
    text = text.replace(old, new)
skill.write_text(text, encoding="utf-8")
# Round 3 (gp-28td): a made-up 16-digit id on the setup page must fail the
# no-Meta-id check. Built here, so no digit run sits in this file.
fake_id = "1" + "0" * 14 + "1"
page = dst / "whatsapp/docs/meta-setup.md"
ptext = page.read_text(encoding="utf-8")
assert "**Houdini**, which" in ptext
page.write_text(ptext.replace("**Houdini**, which", f"**Houdini** ({fake_id}), which", 1), encoding="utf-8")
# Codex's round-3 P2 and P3: each other form in its own file, so each must
# fail on its own line.
forms = {
    "red-e164.md": "+1" + "555" + "0" * 7,
    "red-parens.md": "+1 (" + "555" + ") " + "000-" + "0" * 4,
    "red-grouped.md": " ".join(["1000"] * 3 + ["0001"]),
    "red-touching.md": "id" + fake_id + "x",
}
for name, form in forms.items():
    (dst / "whatsapp/docs" / name).write_text(f"The value {form} is here.\n", encoding="utf-8")
r = subprocess.run([sys.executable, "-I", str(pathlib.Path(__file__).with_name("verbs.py")), str(dst)], capture_output=True, text=True)
fails = [l for l in r.stdout.splitlines() if l.startswith("FAIL")]
print(f"verbs.py rc={r.returncode} on the mutated mirror; {len(fails)} FAIL lines:")
for l in fails:
    print("  " + l)
ok = (r.returncode == 1 and len(fails) == 4 + len(forms)
      and any("clear-gap <conversation>" in l for l in fails)
      and any("nudge (unshifted)" in l for l in fails)
      and any("skill quotes the failed delivery" in l for l in fails)
      and any("meta-setup.md: no Meta id or 555 number" in l for l in fails)
      and all(any(f"{name}: no Meta id or 555 number" in l for l in fails) for name in forms))
print("RED OK" if ok else "RED NOT SEEN")
sys.exit(0 if ok else 1)

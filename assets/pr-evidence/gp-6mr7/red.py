"""RED for verbs.py: mirror whatsapp/ into a scratch root, mutate one quoted
verb form, one keeper quote and the failed-delivery line in the skill, put
recorded Meta values into the setup page and into files of their own (the
table below: one row per form the codex rounds named), add control files
that must pass, and expect verbs.py to fail on exactly the mutations.
The recorded values come from citadel's records at run time and are
written only into the scratch mirror, never into this file; three rows use
a synthetic record written into the scratch root beside the real ones.
Usage: python3 -I red.py <worktree> <scratch-root>"""

import pathlib
import re
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

# Round 3 (gp-28td): one recorded id and the recorded test number.
rec = pathlib.Path("~/.config/citadel-mayor/whatsapp/meta-ids.env").expanduser().read_text(encoding="utf-8")
an_id = re.findall(r"(?<![0-9])[0-9]{16}(?![0-9])", rec)[0]
phone = re.sub(r"[^0-9]", "", re.findall(r"\+1[0-9 ().-]{10,16}", rec)[0])[-10:]
a, b, c = phone[:3], phone[3:6], phone[6:]


def g4(s, sep):
    return sep.join(s[i:i + 4] for i in range(0, len(s), 4))


page = dst / "whatsapp/docs/meta-setup.md"
ptext = page.read_text(encoding="utf-8")
assert "**Houdini**, which" in ptext
page.write_text(ptext.replace("**Houdini**, which", f"**Houdini** ({an_id}), which", 1), encoding="utf-8")
# The spec: every row must FAIL, each on its own line (codex round-3 r1 and r2).
hostile = {
    "red-id-grouped.md": g4(an_id, " "),
    "red-id-nbsp.md": g4(an_id, " "),
    "red-id-double-space.md": g4(an_id, "  "),
    "red-id-dashed.md": g4(an_id, "-"),
    "red-id-touching.md": "id" + an_id + "x",
    "red-id-hexlike.md": "a" + an_id,
    "red-id-url.md": "https://example.invalid/" + an_id + "?v=1",
    "red-id-split-8-8.md": an_id[:8] + "-" + an_id[8:],
    # Codex r3: markup with a digit inside a value the reader sees whole.
    "red-id-link-split.md": f"[{an_id[:8]}](https://example.invalid/v2){an_id[8:]}",
    "red-id-ref-split.md": f"[{an_id[:8]}][9]{an_id[8:]}\n\n[9]: https://example.invalid/\n",
    "red-id-html-split.md": f'{an_id[:8]}<span data-v="2"></span>{an_id[8:]}',
    # Codex r2's exact stamp forms and r3's NBSP-spaced record phone, as
    # values of the synthetic record below.
    "red-synthetic-invalid-stamp.md": "20999999" + "-" + "999999",
    "red-synthetic-prefixed-stamp.md": "1-" + "20261104" + "-" + "235959",
    "red-synthetic-nbsp-phone.md": "212-" + "000-" + "0199",
    "red-phone-e164.md": "+1" + phone,
    "red-phone-e164-touching.md": "+1" + phone + "a",
    "red-phone-parens.md": f"+1 ({a}) {b}-{c}",
    "red-phone-nbsp.md": f"+1 {a} {b} {c}",
    "red-phone-national.md": f"{a}-{b}-{c}",
}
# Controls: each must PASS (no false positive on what the evidence carries).
control = {
    "red-control-commit-id.md": "8062fa2c599095d9d58c7" + "2432663" + "411334d62b6b",
    "red-control-utc-stamp.md": "taylor-" + "20261104" + "-213516Z.md",
    "red-control-epoch-ms.md": "1793" + "835509" + "000",
    "red-control-unrecorded-id.md": "1" + "0" * 14 + "1",
}
for name, form in {**hostile, **control}.items():
    (dst / "whatsapp/docs" / name).write_text(f"The value {form} is here.\n", encoding="utf-8")
# A synthetic record beside the real ones (outside the scanned tree): two
# stamp-shaped values and a phone spaced with non-breaking spaces.
synthetic = dst / "synthetic-record.txt"
synthetic.write_text("A=" + "20999999" + "999999\nB=1" + "20261104" + "235959\n"
                     + "C=+1 212 " + "000 " + "0199\n", encoding="utf-8")
records = [str(pathlib.Path("~/.config/citadel-mayor/whatsapp/meta-ids.env").expanduser()),
           "/Users/tailor512/city/assets/handoff/sessions/2026-11-04-498th.md", str(synthetic)]
r = subprocess.run([sys.executable, "-I", str(pathlib.Path(__file__).with_name("verbs.py")), str(dst), *records], capture_output=True, text=True)
fails = [l for l in r.stdout.splitlines() if l.startswith("FAIL")]
print(f"verbs.py rc={r.returncode} on the mutated mirror; {len(fails)} FAIL lines:")
for l in fails:
    print("  " + l)
value_fail = "no Meta value from citadel's records"
ok = (r.returncode == 1 and len(fails) == 4 + len(hostile)
      and any("clear-gap <conversation>" in l for l in fails)
      and any("nudge (unshifted)" in l for l in fails)
      and any("skill quotes the failed delivery" in l for l in fails)
      and any(f"meta-setup.md: {value_fail}" in l for l in fails)
      and all(any(f"{name}: {value_fail}" in l for l in fails) for name in hostile)
      and not any(name in l for l in fails for name in control))
print("RED OK" if ok else "RED NOT SEEN")
sys.exit(0 if ok else 1)

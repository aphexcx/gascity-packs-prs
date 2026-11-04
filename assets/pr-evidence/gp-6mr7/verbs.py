"""Check the surface's quoted strings against bead A's sources.

1. Every `gc whatsapp ...` form quoted in a code span or fenced block of the
   skill, the fragment and the docs must appear byte for byte in one of
   A's help.md files (whatsapp/commands/**/help.md), after trimming a
   trailing "`" or code-fence context. A form may be a prefix of a help line
   (e.g. "gc whatsapp publish --update" names the flag) only when it ends at
   a token boundary of that help line.
2. The keeper event texts, the redelivery marker, the failed-delivery line,
   the reply line, the attachment block and the actor name the skill quotes
   must appear in A's
   adapter source (keeper.js, render.js, index.js, media.js) as literal
   fragments, with the skill's <placeholders> standing for ${...} parts.
3. No Meta id and no 555 number in this PR's own files (round 3, gp-28td:
   the repo is public; the ids live in citadel's records), in
   whatsapp/docs, whatsapp/skills, whatsapp/template-fragments and
   assets/pr-evidence/gp-6mr7. Whole hex hashes (commit ids, sha256
   prefixes: hex tokens with at least one letter) and compact UTC stamps
   (YYYYMMDD-HHMMSS, as in the city's record file names) are taken out
   first; then no run of 13 or more digits, also when grouped by single
   spaces, dashes or dots or when touching letters, and no 555 phone number
   in any of the +1 555 NNN NNNN, +1555NNNNNNN, (555) NNN-NNNN or
   555-NNN-NNNN forms. A's adapter tests are out of scope: their 13-digit
   runs are epoch milliseconds.

Usage: python3 -I verbs.py <worktree>
Exit 0 when every check passes; 1 otherwise. Prints one line per check.
"""

import pathlib
import re
import sys

root = pathlib.Path(sys.argv[1])
surface = [
    root / "whatsapp/skills/whatsapp/SKILL.md",
    root / "whatsapp/template-fragments/whatsapp-v0.template.md",
    root / "whatsapp/docs/owner-onboarding.md",
    root / "whatsapp/docs/meta-setup.md",
    root / "whatsapp/docs/README.md",
]
helps = sorted((root / "whatsapp/commands").rglob("help.md"))
help_text = "\n".join(p.read_text(encoding="utf-8") for p in helps)
help_lines = [l.strip() for l in help_text.splitlines()]

failures = 0


def report(ok, what):
    global failures
    print(("PASS " if ok else "FAIL ") + what)
    if not ok:
        failures += 1


def quoted_forms(text):
    forms = []
    in_fence = False
    for line in text.splitlines():
        if line.strip().startswith("```"):
            in_fence = not in_fence
            continue
        if in_fence:
            if line.strip().startswith("gc whatsapp"):
                forms.append(line.strip())
            continue
        for span in re.findall(r"`([^`]+)`", line):
            if span.startswith("gc whatsapp"):
                forms.append(span.strip())
    # Inline spans broken across a line wrap: rejoin "`gc whatsapp\n x`".
    for span in re.findall(r"`(gc whatsapp[^`]*)`", text.replace("\n", " ")):
        forms.append(re.sub(r"\s+", " ", span).strip())
    return sorted(set(forms))


def form_ok(form):
    if form in help_text:
        # Prefix of a help line must end on a token boundary.
        for hl in help_lines:
            i = hl.find(form)
            if i >= 0:
                end = i + len(form)
                if end == len(hl) or hl[end] in " `|,.)":
                    return True
        return False
    return False


for f in surface:
    text = f.read_text(encoding="utf-8")
    for form in quoted_forms(text):
        report(form_ok(form), f"{f.relative_to(root)}: verb form {form!r} in A's help.md")

src = {n: (root / "whatsapp/adapter/src" / n).read_text(encoding="utf-8")
       for n in ("keeper.js", "render.js", "index.js", "media.js")}
alljs = "\n".join(src.values())
# NUDGE_TAIL / CLOSED_TAIL are constants spliced into the templates.
consts = dict(re.findall(r"const (NUDGE_TAIL|CLOSED_TAIL) = '([^']*)';", src["keeper.js"]))
expanded = alljs.replace("${NUDGE_TAIL}", consts.get("NUDGE_TAIL", "")).replace("${CLOSED_TAIL}", consts.get("CLOSED_TAIL", ""))
# The daily text is `${head} Window open, ...` with head a template literal
# defined on the line above: splice it in.
head = re.search(r"const head = `([^`]*)`;", src["keeper.js"])
if head:
    expanded = expanded.replace("${head}", head.group(1))


def literal_in_source(quote):
    # The skill's <placeholders> stand for ${...} in the source; "<N>
    # minutes" is one ${left} value (minutesLeft() renders "N minutes").
    # Every literal piece between placeholders must appear, in order, in one
    # source line (after the constants are spliced in).
    pieces = [p for p in re.split(r"<[^>]+>(?: minutes)?", quote) if p]
    for line in expanded.splitlines():
        pos = 0
        good = True
        for p in pieces:
            j = line.find(p, pos)
            if j < 0:
                good = False
                break
            pos = j + len(p)
        if good:
            return True
    return False


# Markdown renders a wrapped inline span with a space: compare on that.
skill = re.sub(r"\s+", " ", (root / "whatsapp/skills/whatsapp/SKILL.md").read_text(encoding="utf-8"))
frag = re.sub(r"\s+", " ", (root / "whatsapp/template-fragments/whatsapp-v0.template.md").read_text(encoding="utf-8"))
quotes = {
    "daily update (window open)": "[window-keeper] <wa_id>: daily update due. Send today's short business update with one acknowledgement ask. Window open, closes <time>.",
    "daily update (window closed)": "Window closed since <time>",
    "nudge (unshifted)": "[window-keeper] <wa_id>: window closes in <N> minutes, nobody from the business has written since <time>. One short line asking anyone in the group to reply.",
    "nudge (shifted)": "window closes at <time>, after your hours; last chance before close of business in <N> minutes.",
    "redelivery marker": "[redelivery <wamid>: a first copy may have reached you; if you already answered it, do nothing]",
    "actor": "Dynasty window-keeper",
    "reply line": "gc whatsapp publish --to <wa_id> --text-file <path>",
    "attachment line": "saved to <path>; Read that path to view it",
    "voice transcript": "[voice note transcript: <name>]",
    "reaction": "[reaction <emoji> to <message id>]",
    # Round 2 (gp-awtb, the read's MINOR 5): render.js:80-83's failed status.
    "failed delivery": "[delivery failed: <code> <title>]",
}
for what, q in quotes.items():
    report(q in skill, f"skill quotes the {what}")
    report(literal_in_source(q), f"A's adapter source carries the {what}: {q!r}")

for q in ("[window-keeper] <wa_id>: daily update due.", "[window-keeper] <wa_id>: window closes in <N> minutes,", "[redelivery <wamid>:", "[delivery failed:"):
    report(q in frag, f"fragment quotes {q!r}")

afik = "only if there is an actual update"
report(afik in skill and afik in frag, "Afik's words quoted in the skill and the fragment")
ask = "reply ok so I can keep this line open"
report(ask in skill and ask in frag, "the keeper's one ask quoted in the skill and the fragment")

hex_hash = re.compile(r"(?<![0-9A-Za-z])(?=[0-9a-f]*[a-f])[0-9a-f]{7,64}(?![0-9A-Za-z])")
utc_stamp = re.compile(r"(?<![0-9])20[0-9]{6}-[0-9]{6}(?![0-9])")
meta_id = re.compile(r"(?<![0-9])[0-9](?:[ .-]?[0-9]){12,}(?![0-9])")
test_number = re.compile(r"(?<![0-9])(?:\+?1[ .-]?)?\(?555\)?[ .-]?[0-9]{3}[ .-]?[0-9]{4}(?![0-9])")
own = ("whatsapp/docs", "whatsapp/skills", "whatsapp/template-fragments", "assets/pr-evidence/gp-6mr7")
for f in sorted(p for d in own if (root / d).is_dir() for p in (root / d).rglob("*") if p.is_file()):
    text = utc_stamp.sub(" ", hex_hash.sub(" ", f.read_text(encoding="utf-8", errors="replace")))
    hits = sorted(set(meta_id.findall(text) + test_number.findall(text)))
    report(not hits, f"{f.relative_to(root)}: no Meta id or 555 number" + (f" (found {len(hits)})" if hits else ""))

sys.exit(1 if failures else 0)

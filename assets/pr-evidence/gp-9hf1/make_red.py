"""Build the two RED variants of the adapter in scratch copies (never the repo).

red1: the pre-round-8 RULE 1 (design line 114 at bc2fd14): every watermark is
read at start only; the in-doubt rule reads the entry's own conversation's
watermark; no scheduled read, no read after a poll, no read before a plain post.

red2: the pre-round-8 RULE 2 (design line 192 at bc2fd14): compaction drops
acknowledged AND dead entries; no replay of a dead-letter line without its
journal line.
"""
import pathlib
import shutil
import sys

src = pathlib.Path(sys.argv[1])
out = pathlib.Path(sys.argv[2])


def copy(name):
    dest = out / name
    if dest.exists():
        shutil.rmtree(dest)
    shutil.copytree(src, dest)
    return dest


def replace_once(path, old, new):
    text = path.read_text()
    count = text.count(old)
    if count != 1:
        raise SystemExit(f"{path}: expected one occurrence, found {count}: {old[:60]!r}")
    path.write_text(text.replace(old, new))


red1 = copy("red1")
inbound = red1 / "src" / "inbound.js"
replace_once(inbound,
             "  async function readWatermarksOnce(reason) {\n    watermarkRunning = true;\n    try {\n      for (const [conversationId, wm] of [...journal.watermarks]) {",
             "  async function readWatermarksOnce(reason, only) {\n    watermarkRunning = true;\n    try {\n      for (const [conversationId, wm] of [...journal.watermarks]) {\n        if (only && conversationId !== only) continue;")
replace_once(inbound,
             "  function readAllWatermarks(reason) {\n    const run = watermarkChain.catch(() => {}).then(() => readWatermarksOnce(reason));",
             "  function readAllWatermarks(reason, only) {\n    const run = watermarkChain.catch(() => {}).then(() => readWatermarksOnce(reason, only));")
replace_once(inbound,
             "    await readAllWatermarks('in-doubt');",
             "    await readAllWatermarks('in-doubt', e.waId);")
replace_once(inbound,
             "    await readAllWatermarks('before-plain-post');\n    if (journal.gaps.size > 0) return redeliver(rid);",
             "    if (journal.gaps.has(e.waId)) return redeliver(rid);")
replace_once(inbound,
             "        // Rule 1: the all-watermark read runs after every poll that returned a delivery.\n        watermarkWanted = true;\n",
             "")
replace_once(inbound,
             "      && (watermarkWanted || now >= (stats.lastWatermarkReadAt ?? 0) + watermarkCheckMs)) {",
             "      && watermarkWanted) {")
replace_once(inbound,
             "    if (present || e.everAccepted || journal.gaps.size > 0) {",
             "    if (present || e.everAccepted || journal.gaps.has(e.waId)) {")

red2 = copy("red2")
journal = red2 / "src" / "journal.js"
replace_once(journal,
             "        if (e && !e.acknowledged) kept.push(line);\n      } else if (line.t === 'probe') {",
             "        if (e && !e.acknowledged && !e.dead) kept.push(line);\n      } else if (line.t === 'probe') {")
replace_once(journal,
             "      if (e.acknowledged) { fold.entries.delete(rid); dropped += 1; }",
             "      if (e.acknowledged || e.dead) { fold.entries.delete(rid); dropped += 1; }")
inbound2 = red2 / "src" / "inbound.js"
replace_once(inbound2,
             "    for (const line of deadLetters.entries()) {",
             "    for (const line of []) {")
print("red variants built:", red1, red2)

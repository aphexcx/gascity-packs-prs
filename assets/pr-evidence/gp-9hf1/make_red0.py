"""red0: the engine modules replaced by stubs that export the same names and
implement nothing, so every engine test runs RED against the absence of the
implementation (the TDD baseline). Built in a scratch copy, never the repo."""
import pathlib
import shutil
import sys

src = pathlib.Path(sys.argv[1])
dest = pathlib.Path(sys.argv[2])
if dest.exists():
    shutil.rmtree(dest)
shutil.copytree(src, dest)
S = dest / "src"

NI = "() => { throw new Error('not implemented'); }"
stubs = {
    "inbound.js": f"""export const READBACK_FAST_MS = 0;
export const NO_ROUTE_ON_MARKED_NOTE = '';
export const verdictOf = {NI};
export const readbackInterval = {NI};
export function createInbound() {{
  return {{ tick: async () => {{}}, resolve: async () => ({{ status: 501, json: {{}} }}), clearGap: async () => ({{ status: 501, json: {{}} }}), status: () => ({{}}), stop() {{}}, authFailed: false }};
}}
""",
    "journal.js": f"""export const JOURNAL_FILE = 'inbound_journal.jsonl';
export const emptyFold = {NI};
export const applyLine = {NI};
export const entryState = {NI};
export const ackOwed = {NI};
export const readLines = () => ({{ lines: [], tornAt: -1 }});
export const foldLines = {NI};
export function openJournal() {{ return {{ entries: new Map(), watermarks: new Map(), gaps: new Map(), fold: {{ probes: [] }}, append() {{}}, compact() {{ return {{ lines: 0, droppedEntries: 0 }}; }}, close() {{}} }}; }}
""",
    "evidence.js": f"""export const probeShape = {NI};
export const turnHoldsPaste = {NI};
export const pasteFound = {NI};
""",
    "render.js": f"""export const providerMessageId = {NI};
export const ridFor = {NI};
export const pmidOfRid = {NI};
export const lastFour = {NI};
export const statusOf = {NI};
export const markerLine = {NI};
export const inboundAtMs = {NI};
export const renderText = {NI};
export const renderMessage = {NI};
""",
    "deadletter.js": f"""export const safeWaId = {NI};
export function createDeadLetters() {{ return {{ has: () => false, get: () => undefined, entries: () => [], count: () => 0, write: () => false }}; }}
""",
}
for name, body in stubs.items():
    (S / name).write_text("// red0 stub: not implemented\n" + body)
print("red0 built:", dest)

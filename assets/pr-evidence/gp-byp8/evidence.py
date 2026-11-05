"""gp-byp8 evidence (PR 76 round 4): GREEN, typecheck, the wrangler dry run, the RED and the deploy gap.

Usage: python3 -I evidence.py <worktree root> <pnpm path> <out dir>

- GREEN (vitest-green.txt): the committed tree.
- RED (vitest-red-base.txt): the committed tests against PR 76's round-3 head (e39c0b90): its src/
  and its 0003. Every gp-byp8 test that pins a change must fail, and nothing else.
- RED B (vitest-red-first-cut.txt): the committed tests against the round's first cut (17a54f6e), before
  the codex self-review r1 fix. Only that fix's tests fail (and the later fixes' tests that need it).
- RED C (vitest-red-second-cut.txt): the committed tests against d520e098, before the lane's fresh
  self-review r1 fix. Only that fix's tests fail.
- RED D (vitest-red-third-cut.txt): the committed tests against 3b1ba734, before the codex self-review r3
  fix. Only that fix's test fails.
- RED E (vitest-red-fourth-cut.txt): the committed tests against 22dd1294, before the codex self-review r4
  fix. Only that fix's tests fail.
- Deploy gap (deploy-gap.txt): the deployed Worker's src/ (PR 73's head 392f9c09, the gateway source
  of main 54f53be7) on migrations 0001, 0002 and this round's 0003, with gp-ac0u's and gp-0g2s's
  checks.

Each step writes older files into the worktree and runs. It then writes the committed files back
and verifies each one by sha256 against HEAD's blob. The deploy-gap tests are copied into test/
for their run and moved out to the out dir afterwards. The script deletes nothing.
"""
import datetime
import hashlib
import pathlib
import re
import shutil
import subprocess
import sys

root = pathlib.Path(sys.argv[1])
pnpm = sys.argv[2]
out = pathlib.Path(sys.argv[3])
out.mkdir(parents=True, exist_ok=True)
gw = root / "whatsapp" / "gateway"
BASE = "e39c0b9040605cdb41de9d0d994747c44846b628"  # PR 76's round-3 head: this round's base
FIRST_CUT = "17a54f6e617a1d7cd42daebf7ed331dac7f325ca"  # the round's first cut, before the codex self-review r1 fix
SECOND_CUT = "d520e0981"  # the codex self-review r1 fix, before the lane's fresh self-review r1 fix
THIRD_CUT = "3b1ba7348"  # the fresh self-review r1 fix, before the codex self-review r3 fix
FOURTH_CUT = "22dd1294"  # the codex self-review r3 fix, before the codex self-review r4 fix
DEPLOYED = "392f9c09649dd37eea414dbc520270ba586ed18c"  # PR 73's head: the running Worker
SRC = ["src/gateway.ts", "src/meta.ts", "src/window.ts", "src/index.ts"]
MIG = "migrations/0003_retries.sql"
TEST = "test/bsuid.test.ts"
TRACKED = [*SRC, MIG, TEST]
ANSI = re.compile(r"\x1b\[[0-9;]*m")


def git(*args: str) -> bytes:
    return subprocess.run(["git", "-C", str(root), *args], capture_output=True, check=True).stdout


def blob(rev: str, rel: str) -> bytes:
    return git("show", f"{rev}:whatsapp/gateway/{rel}")


def sha(b: bytes) -> str:
    return hashlib.sha256(b).hexdigest()


def head() -> str:
    return git("rev-parse", "HEAD").decode().strip()


def run(args: list[str], title: str, name: str, tree: str) -> int:
    started = datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
    p = subprocess.run([pnpm, *args], cwd=gw, capture_output=True, text=True, timeout=900)
    text = ANSI.sub("", p.stdout + p.stderr)
    files = {rel: sha((gw / rel).read_bytes())[:12] for rel in TRACKED}
    header = (
        f"# {title}\n# HEAD {head()}\n# tree: {tree}\n"
        f"# {started} pnpm {' '.join(args)} (cwd whatsapp/gateway) -> exit {p.returncode}\n"
        f"# sha256: {files}\n\n"
    )
    (out / name).write_text(header + text)
    tail = [line.strip() for line in text.splitlines() if line.strip().startswith(("Tests ", "Test Files "))]
    print(f"{name}: exit {p.returncode} {' | '.join(tail)}")
    return p.returncode


def verify_committed() -> None:
    bad = [rel for rel in TRACKED if sha((gw / rel).read_bytes()) != sha(blob("HEAD", rel))]
    if bad:
        raise SystemExit(f"the tree differs from HEAD in {bad}: stopping")
    print("src/, 0003 and the BSUID tests verified against HEAD by sha256")


def write(rev: str, rels: list[str]) -> None:
    for rel in rels:
        (gw / rel).write_bytes(blob(rev, rel))


verify_committed()
saved = {rel: (gw / rel).read_bytes() for rel in TRACKED}


def put_back() -> None:
    for rel, b in saved.items():
        (gw / rel).write_bytes(b)


VITEST = ["exec", "vitest", "run", "--reporter=verbose"]
run(VITEST, "gp-byp8 GREEN: the committed tree", "vitest-green.txt", "HEAD as committed")
run(["typecheck"], "gp-byp8 typecheck (tsc --noEmit)", "typecheck.txt", "HEAD as committed")
run(["exec", "wrangler", "deploy", "--dry-run", "--outdir", "dist"], "gp-byp8 wrangler dry run (no login)", "wrangler-dry-run.txt", "HEAD as committed")

try:
    write(BASE, [*SRC, MIG])
    run(VITEST, f"gp-byp8 RED: the committed tests against PR 76's round-3 head ({BASE[:8]})", "vitest-red-base.txt",
        f"src/ and {MIG} from {BASE}; the tests, vitest.config.ts and the rest as committed")
    put_back()
    write(FIRST_CUT, SRC)
    run(VITEST, f"gp-byp8 RED B: the committed tests against the first cut's src/ ({FIRST_CUT[:8]}, before the codex self-review r1 fix)",
        "vitest-red-first-cut.txt", f"src/ from {FIRST_CUT}; 0003, the tests and the rest as committed")
    put_back()
    write(SECOND_CUT, SRC)
    run(VITEST, f"gp-byp8 RED C: the committed tests against the second cut's src/ ({SECOND_CUT[:8]}, before the self-review r1 fix)",
        "vitest-red-second-cut.txt", f"src/ from {SECOND_CUT}; 0003, the tests and the rest as committed")
    put_back()
    write(THIRD_CUT, SRC)
    run(VITEST, f"gp-byp8 RED D: the committed tests against the third cut's src/ ({THIRD_CUT[:8]}, before the codex self-review r3 fix)",
        "vitest-red-third-cut.txt", f"src/ from {THIRD_CUT}; 0003, the tests and the rest as committed")
    put_back()
    write(FOURTH_CUT, SRC)
    run(VITEST, f"gp-byp8 RED E: the committed tests against the fourth cut's src/ ({FOURTH_CUT[:8]}, before the codex self-review r4 fix)",
        "vitest-red-fourth-cut.txt", f"src/ from {FOURTH_CUT}; 0003, the tests and the rest as committed")
finally:
    put_back()
verify_committed()

checks = {
    gw / "test" / "deploy-gap-ac0u.test.ts": root / "assets" / "pr-evidence" / "gp-ac0u" / "deploy-gap.check.ts",
    gw / "test" / "deploy-gap-0g2s.test.ts": root / "assets" / "pr-evidence" / "gp-0g2s" / "deploy-gap.check.ts",
}
try:
    write(DEPLOYED, SRC)
    for dest, source in checks.items():
        shutil.copyfile(source, dest)
    run([*VITEST, *[f"test/{d.name}" for d in checks]],
        f"gp-byp8 deploy gap: the deployed Worker's src/ ({DEPLOYED[:8]}) on migrations 0001 + 0002 + this round's 0003",
        "deploy-gap.txt", f"src/ from {DEPLOYED}; the migrations as committed; the two checks copied into test/")
finally:
    for dest in checks:
        if dest.exists():
            shutil.move(str(dest), str(out / (dest.name + ".ran")))
    put_back()
verify_committed()

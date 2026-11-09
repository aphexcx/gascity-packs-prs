"""gp-w376 evidence (PR 76 round 3): GREEN, typecheck, the wrangler dry run, the REDs, the partial
run and the deploy gap.

Usage: python3 -I evidence.py <worktree root> <pnpm path> <out dir>

- GREEN: the committed tree.
- RED A (vitest-red-base.txt): the committed tests against PR 76's round-2 head (ba82ab46): its src/
  and its 0003 (no backfill). Every gp-w376 test must fail, and nothing else.
- RED B (vitest-red-addendum.txt): the committed tests against e6e5b866's src/ (the two gate
  findings fixed, the Fable read's other findings not). Only the tests the ADDENDUM changed fail.
- RED C (vitest-red-self-review.txt): the committed tests against eaf93d2b's src/ (before the lane's
  self-review fix 1d85f5ba). Only the self-review's tests fail.
- Partial (vitest-partial-823-only.txt): bc99bac1's tests (the first four alias rows) against
  ba82ab46's src/ with line 823 alone changed (the lost number's start from w). The row whose number
  another row held still fails: the re-key's own change is needed too.
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
BASE = "ba82ab461d4767a3b9b652991446d748741e8f5b"  # PR 76's round-2 head: this round's base
SCOPED = "e6e5b866"  # the two gate findings fixed; the ADDENDUM not yet
ALIAS = "bc99bac1"  # the alias-span commit: its four-row table
REVIEWED = "eaf93d2b"  # the head the lane's self-review read; 1d85f5ba answers it
DEPLOYED = "392f9c09649dd37eea414dbc520270ba586ed18c"  # PR 73's head: the running Worker
SRC = ["src/gateway.ts", "src/meta.ts", "src/window.ts", "src/index.ts"]
MIG = "migrations/0003_retries.sql"
TEST = "test/bsuid.test.ts"
TRACKED = [*SRC, MIG, TEST]
LINE_823 = ("UNION ALL SELECT ?3, b FROM o WHERE ?3 IS NOT o.wa_id", "UNION ALL SELECT ?3, w FROM o WHERE ?3 IS NOT o.wa_id")
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
run(VITEST, "gp-w376 GREEN: the committed tree", "vitest-green.txt", "HEAD as committed")
run(["typecheck"], "gp-w376 typecheck (tsc --noEmit)", "typecheck.txt", "HEAD as committed")
run(["exec", "wrangler", "deploy", "--dry-run", "--outdir", "dist"], "gp-w376 wrangler dry run (no login)", "wrangler-dry-run.txt", "HEAD as committed")

try:
    write(BASE, [*SRC, MIG])
    run(VITEST, f"gp-w376 RED A: the committed tests against PR 76's round-2 head ({BASE[:8]})", "vitest-red-base.txt",
        f"src/ and {MIG} from {BASE}; the tests, vitest.config.ts and the rest as committed")
    put_back()
    write(SCOPED, SRC)
    run(VITEST, f"gp-w376 RED B: the committed tests against {SCOPED}'s src/ (the ADDENDUM's findings unfixed)",
        "vitest-red-addendum.txt", f"src/ from {SCOPED}; 0003, the tests and the rest as committed")
    put_back()
    write(REVIEWED, SRC)
    run(VITEST, f"gp-w376 RED C: the committed tests against {REVIEWED}'s src/ (the self-review's findings unfixed)",
        "vitest-red-self-review.txt", f"src/ from {REVIEWED}; 0003, the tests and the rest as committed")
    put_back()
    write(BASE, [*SRC, MIG])
    write(ALIAS, [TEST])
    g = gw / "src/gateway.ts"
    text = g.read_text()
    if text.count(LINE_823[0]) != 1:
        raise SystemExit("line 823's pattern is not in the base source once: stopping")
    g.write_text(text.replace(*LINE_823))
    run(VITEST, f"gp-w376 partial: {ALIAS}'s tests against {BASE[:8]}'s src/ with line 823 alone changed",
        "vitest-partial-823-only.txt",
        f"src/ and {MIG} from {BASE}, line 823's `b` read as `w`; {TEST} from {ALIAS}; the rest as committed")
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
        f"gp-w376 deploy gap: the deployed Worker's src/ ({DEPLOYED[:8]}) on migrations 0001 + 0002 + this round's 0003",
        "deploy-gap.txt", f"src/ from {DEPLOYED}; the migrations as committed; the two checks copied into test/")
finally:
    for dest in checks:
        if dest.exists():
            shutil.move(str(dest), str(out / (dest.name + ".ran")))
    put_back()
verify_committed()

"""gp-ac0u evidence: GREEN, typecheck, the wrangler dry run, the final RED against the stack base's
source, and the deploy-gap check (the v1 Worker on the v2 schema).

Usage: python3 -I evidence.py <worktree root> <pnpm path> <out dir>

The RED and deploy-gap steps write the stack base's src/*.ts into the worktree and run. They then
write the committed files back and verify each one by sha256 against HEAD's blob. For the RED run,
0002_bsuid.sql is renamed to 0002_bsuid.sql.off and back. The deploy-gap test is copied into
test/ for its run and moved out to the out dir afterwards. The script deletes nothing.
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
here = pathlib.Path(__file__).resolve().parent
BASE = "392f9c09649dd37eea414dbc520270ba586ed18c"
SRC = ["src/gateway.ts", "src/meta.ts", "src/window.ts"]
MIG = "migrations/0002_bsuid.sql"
ANSI = re.compile(r"\x1b\[[0-9;]*m")


def git(*args: str) -> bytes:
    return subprocess.run(["git", "-C", str(root), *args], capture_output=True, check=True).stdout


def blob(rev: str, rel: str) -> bytes:
    return git("show", f"{rev}:whatsapp/gateway/{rel}")


def sha(b: bytes) -> str:
    return hashlib.sha256(b).hexdigest()


def head() -> str:
    return git("rev-parse", "HEAD").decode().strip()


def run(args: list[str], title: str, name: str) -> int:
    started = datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
    p = subprocess.run([pnpm, *args], cwd=gw, capture_output=True, text=True, timeout=900)
    text = ANSI.sub("", p.stdout + p.stderr)
    header = f"# {title}\n# HEAD {head()}\n# {started} pnpm {' '.join(args)} (cwd whatsapp/gateway) -> exit {p.returncode}\n\n"
    (out / name).write_text(header + text)
    tail = [line.strip() for line in text.splitlines() if line.strip().startswith(("Tests ", "Test Files "))]
    print(f"{name}: exit {p.returncode} {' | '.join(tail)}")
    return p.returncode


def verify_committed() -> None:
    bad = [rel for rel in [*SRC, MIG] if sha((gw / rel).read_bytes()) != sha(blob("HEAD", rel))]
    if bad:
        raise SystemExit(f"the tree differs from HEAD in {bad}: stopping")
    print("src/ and 0002 verified against HEAD by sha256")


verify_committed()
run(["exec", "vitest", "run", "--reporter=verbose"], "gp-ac0u GREEN: the committed tree", "vitest-green.txt")
run(["typecheck"], "gp-ac0u typecheck (tsc --noEmit)", "typecheck.txt")
run(["exec", "wrangler", "deploy", "--dry-run", "--outdir", "dist"], "gp-ac0u wrangler dry run (no login)", "wrangler-dry-run.txt")

saved = {rel: (gw / rel).read_bytes() for rel in SRC}
mig, off = gw / MIG, gw / (MIG + ".off")
try:
    for rel in SRC:
        (gw / rel).write_bytes(blob(BASE, rel))
    mig.rename(off)
    run(
        ["exec", "vitest", "run", "--reporter=verbose"],
        f"gp-ac0u RED (final): the committed tests against the stack base's src/ ({BASE[:8]}), 0002 absent",
        "vitest-red-final.txt",
    )
finally:
    if off.exists():
        off.rename(mig)
    for rel, b in saved.items():
        (gw / rel).write_bytes(b)
verify_committed()

gap = gw / "test" / "deploy-gap.test.ts"
try:
    for rel in SRC:
        (gw / rel).write_bytes(blob(BASE, rel))
    shutil.copyfile(here / "deploy-gap.check.ts", gap)
    run(
        ["exec", "vitest", "run", "--reporter=verbose", "test/deploy-gap.test.ts"],
        f"gp-ac0u deploy gap: the stack base's src/ ({BASE[:8]}, the running Worker) on migrations 0001 + 0002",
        "deploy-gap.txt",
    )
finally:
    if gap.exists():
        shutil.move(str(gap), str(out / "deploy-gap.test.ts.ran"))
    for rel, b in saved.items():
        (gw / rel).write_bytes(b)
verify_committed()

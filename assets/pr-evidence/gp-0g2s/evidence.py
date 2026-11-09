"""gp-0g2s evidence: GREEN, typecheck, the wrangler dry run, two REDs against the stack base's source,
and the deploy gap (the deployed Worker on the v3 schema).

Usage: python3 -I evidence.py <worktree root> <pnpm path> <out dir>

- RED final: the committed tests against the stack base's src/ (PR 76's head), 0003 absent.
- RED on the schema: the same, with 0003 present, so no failure is a missing table.
- Deploy gap: the deployed Worker's src/ (PR 73's head, the gateway source of main 54f53be7) on
  migrations 0001, 0002 and 0003. It runs gp-ac0u's check and this round's.

The RED and deploy-gap steps write older src/*.ts into the worktree and run. They then write the
committed files back and verify each one by sha256 against HEAD's blob. For RED final,
0003_retries.sql is renamed to 0003_retries.sql.off and back. The deploy-gap tests are copied into
test/ for their run and moved out to the out dir afterwards. The script deletes nothing.
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
BASE = "1e0a2483ebaf7bfa8783d28cbc128fd37db10e5a"  # PR 76's head: this round's stack base
DEPLOYED = "392f9c09649dd37eea414dbc520270ba586ed18c"  # PR 73's head: the running Worker
SRC = ["src/gateway.ts", "src/meta.ts", "src/window.ts", "src/index.ts"]
MIG = "migrations/0003_retries.sql"
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
    src = {rel: sha((gw / rel).read_bytes())[:12] for rel in SRC}
    header = (
        f"# {title}\n# HEAD {head()}\n# {started} pnpm {' '.join(args)} (cwd whatsapp/gateway) -> exit {p.returncode}\n"
        f"# src sha256: {src}\n# 0003 present: {(gw / MIG).exists()}\n\n"
    )
    (out / name).write_text(header + text)
    tail = [line.strip() for line in text.splitlines() if line.strip().startswith(("Tests ", "Test Files "))]
    print(f"{name}: exit {p.returncode} {' | '.join(tail)}")
    return p.returncode


def verify_committed() -> None:
    bad = [rel for rel in [*SRC, MIG] if sha((gw / rel).read_bytes()) != sha(blob("HEAD", rel))]
    if bad:
        raise SystemExit(f"the tree differs from HEAD in {bad}: stopping")
    print("src/ and 0003 verified against HEAD by sha256")


def write_src(rev: str) -> None:
    for rel in SRC:
        (gw / rel).write_bytes(blob(rev, rel))


verify_committed()
run(["exec", "vitest", "run", "--reporter=verbose"], "gp-0g2s GREEN: the committed tree", "vitest-green.txt")
run(["typecheck"], "gp-0g2s typecheck (tsc --noEmit)", "typecheck.txt")
run(["exec", "wrangler", "deploy", "--dry-run", "--outdir", "dist"], "gp-0g2s wrangler dry run (no login)", "wrangler-dry-run.txt")

saved = {rel: (gw / rel).read_bytes() for rel in SRC}
mig, off = gw / MIG, gw / (MIG + ".off")
try:
    write_src(BASE)
    mig.rename(off)
    run(
        ["exec", "vitest", "run", "--reporter=verbose"],
        f"gp-0g2s RED (final): the committed tests against the stack base's src/ ({BASE[:8]}), 0003 absent",
        "vitest-red-final.txt",
    )
    off.rename(mig)
    run(
        ["exec", "vitest", "run", "--reporter=verbose"],
        f"gp-0g2s RED on the v3 schema: the committed tests against the stack base's src/ ({BASE[:8]}), 0003 present",
        "vitest-red-schema.txt",
    )
finally:
    if off.exists():
        off.rename(mig)
    for rel, b in saved.items():
        (gw / rel).write_bytes(b)
verify_committed()

checks = {
    gw / "test" / "deploy-gap-ac0u.test.ts": root / "assets" / "pr-evidence" / "gp-ac0u" / "deploy-gap.check.ts",
    gw / "test" / "deploy-gap-0g2s.test.ts": here / "deploy-gap.check.ts",
}
try:
    write_src(DEPLOYED)
    for dest, source in checks.items():
        shutil.copyfile(source, dest)
    run(
        ["exec", "vitest", "run", "--reporter=verbose", *[f"test/{d.name}" for d in checks]],
        f"gp-0g2s deploy gap: the deployed Worker's src/ ({DEPLOYED[:8]}) on migrations 0001 + 0002 + 0003",
        "deploy-gap.txt",
    )
finally:
    for dest in checks:
        if dest.exists():
            shutil.move(str(dest), str(out / (dest.name + ".ran")))
    for rel, b in saved.items():
        (gw / rel).write_bytes(b)
verify_committed()

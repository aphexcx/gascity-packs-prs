"""Release contract with real Git snapshots/checksums and stub Go/GitHub."""
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys

import pytest

SCRIPT = Path(__file__).resolve().parents[1] / "release-pack-binaries.sh"


@pytest.fixture
def release_repo(tmp_path):
    repo = tmp_path / "repo"
    (repo / "scripts").mkdir(parents=True)
    shutil.copy(SCRIPT, repo / "scripts")
    for pack in ("gchat", "slack-full"):
        (repo / pack / "adapter").mkdir(parents=True)
        (repo / pack / "cli").mkdir()
        (repo / pack / "pack.toml").write_text('[pack]\nversion = "1.2.3"\n')
        for module in ("adapter", "cli"):
            (repo / pack / module / "go.mod").write_text("module fixture\n")

    def git(*args):
        return subprocess.check_output(["git", "-C", str(repo), *args], text=True).strip()

    git("init", "-q")
    git("config", "user.name", "Release Test")
    git("config", "user.email", "release@example.invalid")
    git("add", ".")
    git("commit", "-qm", "fixture")
    commit = git("rev-parse", "HEAD")
    stub_bin = tmp_path / "bin"
    stub_bin.mkdir()
    calls = tmp_path / "calls.jsonl"
    stub = f"#!{sys.executable}\n" + '''import json, os, pathlib, sys
name = pathlib.Path(sys.argv[0]).name
with open(os.environ["CALLS"], "a") as stream:
    stream.write(json.dumps({"tool": name, "args": sys.argv[1:],
        "arch": os.environ.get("GOARCH"), "os": os.environ.get("GOOS"),
        "cgo": os.environ.get("CGO_ENABLED"), "cwd": os.getcwd(),
        "version": pathlib.Path("../pack.toml").read_text() if name == "go" else None}) + "\\n")
if name == "go":
    if os.environ.get("BUILD_FAIL"):
        sys.exit(17)
    out = pathlib.Path(sys.argv[sys.argv.index("-o") + 1])
    out.write_bytes((os.environ["GOARCH"] + " ".join(sys.argv[1:])).encode())
    out.chmod(0o755)
elif sys.argv[1] == "api":
    if os.environ.get("API_FAIL"):
        sys.exit(23)
    if os.environ.get("EXISTING_TAG"):
        print("refs/tags/already-exists")
'''
    for tool in ("go", "gh"):
        (stub_bin / tool).write_text(stub)
        (stub_bin / tool).chmod(0o755)
    env = dict(os.environ, PATH=f"{stub_bin}:{os.environ['PATH']}", CALLS=str(calls))

    def run(pack="gchat", *args, **extra_env):
        return subprocess.run([str(repo / "scripts" / SCRIPT.name), pack, *args],
                              cwd=tmp_path, env=dict(env, **extra_env),
                              text=True, capture_output=True, timeout=30)

    def invocations():
        return [json.loads(line) for line in calls.read_text().splitlines()] if calls.exists() else []

    return repo, git, commit, run, invocations


@pytest.mark.parametrize("pack,binaries", [
    ("gchat", {"gc-gchat-adapter"}),
    ("slack-full", {"gc-slack-adapter", "gc-slack-cli"}),
])
def test_both_architectures_checksums_and_pack_version(release_repo, pack, binaries):
    repo, git, commit, run, invocations = release_repo
    # Dirty version must never leak into a release named for HEAD.
    (repo / pack / "pack.toml").write_text('[pack]\nversion = "9.9.9"\n')
    result = run(pack)
    assert result.returncode == 0, result.stderr
    calls = invocations()
    assert len(calls) == 2 * len(binaries)
    assert all(call["tool"] == "go" for call in calls)
    assert {call["arch"] for call in calls} == {"amd64", "arm64"}
    for call in calls:
        assert call["os"] == "linux" and call["cgo"] == "0"
        assert "-trimpath" in call["args"]
        assert "-s -w -X main.Version=1.2.3" in call["args"]
        assert 'version = "1.2.3"' in call["version"]
    short = git("rev-parse", "--short=8", commit)
    rows = result.stdout.splitlines()
    assert rows[0] == "release\tasset\tsha256\tsize_bytes"
    assert len(rows) == 1 + 2 * len(binaries)
    for row in rows[1:]:
        release, binary, digest, size = row.split("\t")
        assert release in {f"{pack}-linux-{arch}-{short}" for arch in ("amd64", "arm64")}
        assert binary in binaries
        asset = repo / "dist" / release / binary
        assert int(size) == asset.stat().st_size
        assert digest == hashlib.sha256(asset.read_bytes()).hexdigest()
        assert asset.with_suffix(".sha256").read_text() == f"{digest}  {binary}\n"


def test_explicit_commit_uses_that_snapshot(release_repo):
    repo, git, commit, run, invocations = release_repo
    (repo / "gchat/pack.toml").write_text('[pack]\nversion = "2.0.0"\n')
    git("commit", "-qam", "later")
    result = run("gchat", "--commit", commit)
    assert result.returncode == 0, result.stderr
    assert commit in result.stderr
    assert all("-s -w -X main.Version=1.2.3" in call["args"] for call in invocations())


@pytest.mark.parametrize("failure", ["EXISTING_TAG", "API_FAIL"])
def test_remote_tag_or_api_failure_stops_before_build(release_repo, failure):
    _, _, _, run, invocations = release_repo
    result = run("gchat", "--publish", **{failure: "1"})
    assert result.returncode != 0
    assert all(call["tool"] == "gh" and call["args"][0] == "api" for call in invocations())


def test_existing_local_tag_is_refused(release_repo):
    _, git, commit, run, invocations = release_repo
    short = git("rev-parse", "--short=8", commit)
    git("tag", f"gchat-linux-amd64-{short}")
    result = run("gchat", "--publish")
    assert result.returncode != 0
    assert "refusing existing tag" in result.stderr
    assert not invocations()


def test_publish_targets_source_commit_with_all_assets(release_repo):
    _, _, commit, run, invocations = release_repo
    result = run("slack-full", "--publish")
    assert result.returncode == 0, result.stderr
    calls = invocations()
    assert [call["tool"] for call in calls] == ["gh", "gh", "go", "go", "go", "go", "gh", "gh"]
    for call in calls[-2:]:
        args = call["args"]
        assert args[:2] == ["release", "create"]
        assert "--prerelease" in args
        assert args[args.index("--target") + 1] == commit
        assert args[args.index("--repo") + 1] == "aphexcx/gascity-packs-prs"
        assert {Path(arg).name for arg in args[-4:]} == {
            "gc-slack-adapter", "gc-slack-adapter.sha256", "gc-slack-cli", "gc-slack-cli.sha256"}


def test_build_failure_never_publishes(release_repo):
    repo, _, _, run, invocations = release_repo
    result = run("gchat", "--publish", BUILD_FAIL="1")
    assert result.returncode != 0
    assert not any(call["args"][:2] == ["release", "create"] for call in invocations())
    assert not (repo / "dist").exists()


@pytest.mark.parametrize("args", [("unknown",), ("gchat", "--commit"), ("gchat", "--typo")])
def test_invalid_arguments(release_repo, args):
    _, _, _, run, invocations = release_repo
    result = run(*args)
    assert result.returncode == 2
    assert not invocations()

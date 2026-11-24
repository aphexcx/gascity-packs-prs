"""Check the published commands using the current repository and disposable tool fixtures."""

import json
import os
import re
import shlex
import subprocess
import sys
from collections import Counter
from pathlib import Path

import pytest


ROOT = Path(__file__).resolve().parents[1]
SKILL = ROOT / ".claude/skills/verify/SKILL.md"
LOOP = 'for script in gastown/tests/test_*.sh; do bash "$script"; done'
REPLAY = "^review 4: archive replay advances in bounded chunks without dropping rows$"
PYTHON_DIRS = ["board-watcher/tests", "wecom/tests", "oversight-rig/assets/scripts",
               "oversight-rig/skills/city-executive-status/tests"]
SECRETS = [
    ".env", ".env.local", "env", ".netrc", "credentials.json", "service-account-ci.json",
    ".gc/slack/install.env", "secrets/bot-token.txt", "secrets/bot-token-worker.txt",
    "gchat/client.json", "gchat/token.json",
    ".beads-credential-key", "whatsapp/gateway/.dev.vars", "adapter.env", ".envrc",
]
BOOT = {"PATH": os.defpath, "HOME": os.devnull, "GIT_CONFIG_NOSYSTEM": "1"}


def files(*patterns):
    result = subprocess.check_output(
        ["git", "ls-files", "-z", "--cached", "--", *patterns],
        cwd=ROOT, env=BOOT,
    )
    return [Path(os.fsdecode(p)) for p in result.split(b"\0") if p]


def split_document(text):
    setup = re.search(r"^```bash\n(verify\(\) \(\n.*?^\)\n)```$", text, re.M | re.S)
    assert setup, "Missing verification function"
    rest = text[:setup.start()] + text[setup.end():]
    fences = re.compile(r"^```sh\n(.*?)^```$", re.M | re.S)
    blocks = [m[1].rstrip("\n") for m in fences.finditer(rest)]
    return setup[1], blocks, fences.sub("", rest)


def check_prose(prose):
    for line in prose.splitlines():
        assert not line.strip().startswith(("~~~", "```")), f"Unexpected command fence: {line}"
        # Prose uses sentences. Bare executable words and inline shell snippets
        # belong in guarded blocks, including tools that are absent on this host.
        start = re.sub(r"^\s*(?:[-*>] |\d+[.)] )*", "", line)
        assert not re.match(r"(?:[a-z_][\w.-]*|[./]\S+)(?:\s|$)", start), f"Unwrapped command: {line}"
        for inline in re.findall(r"`([^`]+)`", line):
            assert len(inline.split()) == 1, f"Inline command outside a guarded block: {inline}"
    for p in re.findall(r"`([\w.-]+/[\w./-]*|[\w.-]+\.toml)`", prose):
        assert (ROOT / p).exists(), f"Missing repository path: {p}"
    for link in re.findall(r"\]\(([^)]+)\)", prose):
        assert (SKILL.parent / link).is_file(), f"Missing link: {link}"


def excluded(text, directory):
    return re.search(rf"^- `{re.escape(directory)}` — \S.+$", text, re.M)


def required_commands(text):
    commands = [["make", "test-gascity", "PYTEST_ARGS=-q -p no:cacheprovider"], ["make", "registry-format-validate"]]
    ci = (ROOT / ".github/workflows/ci.yml").read_text()
    python_suite = re.search(r"^\s*run: (python3 -m pytest .+)$", ci, re.M)[1]
    commands.append([*shlex.split(python_suite), "-p", "no:cacheprovider"])
    commands += [["python3", "validate_registry.py", "--require-git"],
                 ["-C", "follow-through/test", "node", "--test"],
                 ["-C", "reachable/test", "node", "--test", "--test-skip-pattern", REPLAY],
                 ["bash", "-ec", LOOP]]
    for directory in PYTHON_DIRS:
        if not excluded(text, directory):
            commands.append(["-C", directory, "python3", "-m", "pytest", "-q", ".", "-p", "no:cacheprovider"])
    for mod in files("go.mod", "*/go.mod"):
        if mod.parts[0] == "assets":
            continue
        directory = str(mod.parent)
        prefix = [] if directory == "." else ["-C", directory]
        pattern = "." if not prefix else "./..."
        commands.append([*prefix, "go", "vet", pattern])
        args = ["go", "test", "-count=1", "-json"]
        if directory == "slack-full/adapter":
            assert excluded(text, directory), "Explain why the Slack full Go tests run only in CI"
            continue
        if directory == "gchat/adapter":
            if excluded(text, directory):
                continue
            args.append("-timeout=30m")
        commands.append([*prefix, *args, pattern])
    for pack in ("wecom", "whatsapp"):
        directory = f"{pack}/adapter"
        if not excluded(text, f"{directory}/test"):
            script = json.loads((ROOT / directory / "package.json").read_text())["scripts"]["test"]
            args = shlex.split(script)
            if pack == "whatsapp":
                assert args.pop() == "test/*.test.js"
            commands.append(["-C", directory, *args])
    if not excluded(text, "whatsapp/gateway/test"):
        commands.append(["-C", "whatsapp/gateway", "node_modules/.bin/vitest", "run", "test"])
    return Counter(tuple(c) for c in commands)


def test_command_inventory():
    check_inventory(SKILL.read_text(), (ROOT / "Makefile").read_text())


def check_inventory(text, makefile):
    _, blocks, prose = split_document(text)
    check_prose(prose)
    recipes = dict(re.findall(r"^([\w.-]+):[^\n]*\n((?:\t[^\n]*\n)+)", makefile, re.M))
    actual = []
    for block in blocks:
        assert "\n" not in block and block.startswith("verify "), f"Expected one guarded check: {block}"
        words = shlex.split(block, comments=False)[1:]
        if words[:1] == ["make"]:
            assert words[1] in recipes, f"Undefined Makefile target: {block}"
        directory = ROOT / words[1] if words[:1] == ["-C"] else ROOT
        assert directory.is_dir(), f"Missing working directory: {block}"
        for word in words[2:] if words[:1] == ["-C"] else words:
            if re.fullmatch(r"[\w./-]+\.(?:py|cjs|mjs|js|sh)", word):
                assert (directory / word).is_file(), f"Missing script in working directory: {block}"
        actual.append(tuple(words))
    assert Counter(actual) == required_commands(text), f"Missing or unexpected command: {actual}"
    for target, fragments in {
        "test": ["node --test", "$(PYTEST_RUNNER)", "$(PYTEST_DIRS)", "reachable_status", "python_status"],
        "test-gascity": ["$(PYTEST_RUNNER)", "tests gascity/tests"],
        "test-reachable": ["reachable/test", "node --test"],
        "registry-format-validate": ["validate_registry.py"],
    }.items():
        for fragment in fragments:
            assert fragment in recipes[target], f"Incomplete {target} recipe: missing {fragment}"


@pytest.mark.parametrize("target", ["absent-verify-target", "test#typo"])
def test_unknown_make_target_names_the_target(target):
    text = SKILL.read_text().replace("verify make test-gascity ", f"verify make {target} ")
    with pytest.raises(AssertionError, match=re.escape(target)):
        check_inventory(text, (ROOT / "Makefile").read_text())


def test_script_resolves_from_requested_directory():
    text = SKILL.read_text().replace("verify python3 validate_registry.py",
                                     "verify -C follow-through/test python3 validate_registry.py")
    with pytest.raises(AssertionError, match="Missing script in working directory"):
        check_inventory(text, (ROOT / "Makefile").read_text())


def javascript_runner_covers(path, blocks):
    recursive = {"whatsapp/gateway/test", "reachable/test", "follow-through/test"}
    directory = next((str(p) for p in path.parents if str(p) in recursive), None)
    if directory == "whatsapp/gateway/test":
        return (path.suffix in {".js", ".ts", ".tsx", ".jsx", ".cjs", ".mjs"}
                and "verify -C whatsapp/gateway node_modules/.bin/vitest run test" in blocks)
    if directory in {"reachable/test", "follow-through/test"}:
        command = f"verify -C {directory} node --test"
        if directory == "reachable/test":
            command += f" --test-skip-pattern '{REPLAY}'"
        return path.suffix in {".cjs", ".mjs", ".js"} and command in blocks
    for block in blocks:
        words = shlex.split(block)
        if words[1:3] == ["-C", str(path.parent.parent)] and words[3:5] == ["node", "--test"]:
            if words[1:] == ["-C", "whatsapp/adapter", "node", "--test", "--test-concurrency=2"]:
                return path.relative_to(path.parent.parent).match("test/*.test.js")
            if path.relative_to(path.parent.parent).match(words[-1]):
                return True
    return False


@pytest.mark.parametrize("name", ["new.spec.js", "new.test.ts", "test_new.py"])
def test_adapter_node_glob_excludes_uncollected_files(name):
    script = json.loads((ROOT / "whatsapp/adapter/package.json").read_text())["scripts"]["test"]
    blocks = [f"verify -C whatsapp/adapter {script}"]
    assert javascript_runner_covers(Path("whatsapp/adapter/test/new.test.js"), blocks)
    assert not javascript_runner_covers(Path("whatsapp/adapter/test") / name, blocks)


def test_recursive_suite_roots(monkeypatch):
    nested = [Path(name) for name in ("tests/unit/test_example.py", "follow-through/test/nested/more.test.js",
                                      "whatsapp/gateway/test/nested/more.test.ts")]
    tracked = files
    monkeypatch.setitem(globals(), "files", lambda *patterns: (
        nested if ":(glob)**/*_test.go" in patterns else tracked(*patterns)))
    test_suite_coverage()


def test_suite_coverage():
    text = SKILL.read_text()
    _, blocks, _ = split_document(text)
    ci = (ROOT / ".github/workflows/ci.yml").read_text()
    ci_python = set(re.search(r"^\s*run: python3 -m pytest (.+)$", ci, re.M)[1].split())
    makefile = (ROOT / "Makefile").read_text()
    make_python = set(re.search(r"^PYTEST_DIRS \?= (.+)$", makefile, re.M)[1].split())
    modules = {m.parent for m in files("go.mod", "*/go.mod")}
    patterns = ["*_test.go", "test_*.py", "*_test.py", "test_*.sh", "*.bats"]
    patterns += [f"*.{kind}.{ext}" for kind in ("test", "spec") for ext in ("js", "cjs", "mjs", "ts", "tsx", "jsx")]
    tests = files(*(f":(glob)**/{p}" for p in patterns))
    missing = set()
    for path in tests:
        directory = str(path.parent)
        if excluded(text, directory):
            continue
        if path.suffix == ".go":
            module = next((p for p in path.parents if p in modules), None)
            if module is not None and (module != Path(".") or path.parent == module):
                prefix = "verify " if module == Path(".") else f"verify -C {module} "
                if any(b.startswith(prefix + "go test ") for b in blocks) or excluded(text, str(module)):
                    continue
        if path.suffix == ".py" and (not ci_python.isdisjoint(map(str, path.parents)) or
                ("verify make test" in blocks and not make_python.isdisjoint(map(str, path.parents))) or
                any(f"verify -C {p} python3 -m pytest -q . -p no:cacheprovider" in blocks for p in path.parents)):
            continue
        if path.suffix == ".sh" and directory == "gastown/tests" and any(LOOP in b for b in blocks):
            continue
        if path.name == "test_github_app_token.sh" and directory in make_python:
            wrapper = ROOT / path
            if "test_github_app_token.py" in wrapper.read_text() and wrapper.with_suffix(".py").is_file():
                continue
        if javascript_runner_covers(path, blocks):
            continue
        missing.add(str(path))
    assert not missing, "Suites without a runner or reason: " + ", ".join(sorted(missing))


@pytest.fixture
def harness(tmp_path):
    repo = tmp_path / "checkout"
    tools = tmp_path / "bin"
    tools.mkdir()
    home = tmp_path / "caller"
    home.mkdir()
    subprocess.run(["git", "init", "-q", str(repo)], env=BOOT, check=True)
    setup, blocks, _ = split_document(SKILL.read_text())
    for block in blocks:
        words = shlex.split(block.splitlines()[0])
        if words[1:2] == ["-C"]:
            (repo / words[2]).mkdir(parents=True, exist_ok=True)
    adapter = repo / "whatsapp/adapter/test"
    adapter.mkdir(parents=True, exist_ok=True)
    for name in ("tracked.test.js", "extra.test.js"):
        (adapter / name).touch()
    subprocess.run(["git", "add", "--", "whatsapp/adapter/test/tracked.test.js"], cwd=repo, env=BOOT, check=True)
    gastown = repo / "gastown/tests"
    gastown.mkdir(parents=True)
    for i in range(2):
        (gastown / f"test_{i}.sh").write_text("exit 17\n" if not i else "exit 0\n")
    log = tmp_path / "calls"
    for name in ("go", "python3", "make", "node", "jq", "rg", "sed", "mktemp", "date", "bash",
                 "uv", "curl", "brew", "npm", "pnpm"):
        program = tools / name
        program.write_text(
            "#!/bin/sh\n" + f"log={shlex.quote(str(log))}\n" +
            'printf "%s\\n" "$0 $*" >> "$log"\n' +
            'case "${0##*/}:$1" in\n'
            ' go:env) echo /fixture/cache; exit 0;;\n'
            ' python3:-c|jq:--version|rg:--version) exit 0;;\n'
            ' node:-p) case "$2" in *process.execPath) echo "$0";; *) echo 24;; esac; exit 0;;\n'
            ' sed:--version) echo "GNU sed"; exit 0;;\n'
            ' mktemp:--version|date:--version) echo "GNU coreutils"; exit 0;;\n'
            ' mktemp:*) exec /usr/bin/mktemp "$@";;\n'
            ' bash:-c) exit 0;;\n'
            ' bash:-ec) exec /bin/bash "$@";;\n'
            ' bash:gastown/tests/test_*.sh) exec /bin/bash "$@";;\n'
            'esac\n[ ! -e "$log.dispatched" ] || exit 0\n'
            'touch "$log.dispatched"\nexit 17\n'
        )
        program.chmod(0o755)
    env = {"PATH": f"{tools}:{os.defpath}", "HOME": str(home), "TMPDIR": str(tmp_path),
           "GC_BIN": "fixture", "SLACK_BOT_TOKEN": "fixture", "GC_TEMPLATE": "fixture",
           "NEVER_SILENT": "fixture", "GIT_DIR": str(tmp_path / "wrong.git"),
           "GIT_WORK_TREE": str(home), "CDPATH": str(home), "UNEXPECTED": "fixture"}

    def run(command):
        return subprocess.run(["/bin/bash", "-c", setup + "\n" + command], cwd=repo, env=env,
                              capture_output=True, text=True, timeout=15)

    return repo, tools, env, log, run


def check_child_environment(result, env, extra=None):
    assert result.returncode == 0, result.stderr
    child = dict(line.split("=", 1) for line in result.stdout.splitlines())
    for key, value in (extra or {}).items():
        assert child.pop(key) == value
    assert set(child) == {"HOME", "PATH", "TMPDIR", "GOCACHE", "GOMODCACHE", "GOTOOLCHAIN",
                          "GOPROXY", "GOSUMDB", "GC_NO_INSTALL", "GC_TOOLCHAIN_NODE_INSTALL",
                          "GIT_ALLOW_PROTOCOL", "GIT_TERMINAL_PROMPT"}
    assert child["GOTOOLCHAIN"] == "local" and child["GOPROXY"] == child["GOSUMDB"] == "off"
    assert child["GC_TOOLCHAIN_NODE_INSTALL"] == "off" and child["GC_NO_INSTALL"] == "1"
    assert child["GIT_ALLOW_PROTOCOL"] == "file" and child["GIT_TERMINAL_PROMPT"] == "0"
    assert Path(child["HOME"]).parent == Path(env["TMPDIR"]) and not Path(child["HOME"]).exists()


def test_environment_and_checkout(harness):
    repo, _, env, _, run = harness
    check_child_environment(run("verify env"), env)
    module = Path("slack-full/adapter")
    (Path(env["HOME"]) / module).mkdir(parents=True)
    result = run(f"verify -C {module} pwd -P")
    assert result.returncode == 0 and Path(result.stdout.strip()) == repo / module


def test_gateway_offline_environment(harness):
    repo, _, env, _, run = harness
    program = repo / "whatsapp/gateway/node_modules/.bin/vitest"
    program.parent.mkdir(parents=True)
    program.write_text("#!/bin/sh\nexec /usr/bin/env\n")
    program.chmod(0o755)
    # Measure what this host's shebang interpreter itself exports, e.g. PWD.
    # Invoke env by its real name on hosts with a multicall coreutils binary.
    probe = subprocess.run([str(program)], cwd=repo / "whatsapp/gateway", env={"PATH": env["PATH"]},
                           text=True, capture_output=True, check=True)
    defaults = dict(line.split("=", 1) for line in probe.stdout.splitlines())
    del defaults["PATH"]
    result = run("verify -C whatsapp/gateway node_modules/.bin/vitest")
    check_child_environment(result, env, {**defaults, "WRANGLER_SEND_METRICS": "false",
                                          "CLOUDFLARE_CF_FETCH_ENABLED": "false", "NODE_ENV": "test"})


def test_adapter_dispatches_tracked_test_files_only(harness):
    _, _, _, log, run = harness
    result = run("verify -C whatsapp/adapter node --test --test-concurrency=2")
    assert result.returncode == 17
    invocation = [line for line in log.read_text().splitlines() if "/node --test " in line]
    assert len(invocation) == 1
    assert invocation[0].split(" --test ", 1)[1].split() == ["--test-concurrency=2", "test/tracked.test.js"]
    assert result.stderr.splitlines() == ["not run: untracked test/extra.test.js"]


@pytest.mark.parametrize("call", [
    "verify touch started",
    "if verify touch started; then exit 0; else exit $?; fi",
    "verify touch started || exit $?",
    "verify touch started && exit 0",
    "set -o pipefail; verify touch started | cat",
])
def test_scan_failure_reaches_caller(harness, call):
    repo, tools, _, _, run = harness
    scan = tools / "find"
    scan.write_text("#!/bin/sh\nexit 17\n")
    scan.chmod(0o755)
    assert run(call).returncode == 17
    assert not (repo / "started").exists()


@pytest.mark.parametrize("explicit", [True, False])
def test_provisioned_go_caches(harness, explicit):
    repo, tools, env, _, run = harness
    module = repo / "go" / "pkg" / "mod"
    build = repo / "cache" / "go-build"
    for directory in (module, build):
        directory.mkdir(parents=True)
    go = tools / "go"
    go.write_text('#!/bin/sh\ncase "$2" in\n'
                  'GOMODCACHE) echo "${GOMODCACHE:-${GOPATH:-$HOME/go}/pkg/mod}";;\n'
                  'GOCACHE) echo "${GOCACHE:-${XDG_CACHE_HOME:-$HOME/.cache}/go-build}";;\nesac\n')
    if explicit:
        env.update(GOMODCACHE=str(module), GOCACHE=str(build))
    else:
        env.update(GOPATH=str(repo / "go"), XDG_CACHE_HOME=str(build.parent))
    result = run("unset HOME; verify env")
    check_child_environment(result, env)
    child = dict(line.split("=", 1) for line in result.stdout.splitlines())
    assert (child["GOMODCACHE"], child["GOCACHE"]) == (str(module), str(build))


def test_wecom_child_uses_installed_node(harness):
    _, tools, _, log, run = harness
    installed = tools / "installed-node"
    installed.write_text("#!/bin/sh\nexit 0\n")
    installed.chmod(0o755)
    (tools / "node").write_text(
        '#!/bin/sh\n[ "$GC_TOOLCHAIN_NODE_INSTALL" = off ] || { curl; exit 1; }\n'
        f'case "$2" in *process.execPath) echo {shlex.quote(str(installed))};; *) echo 24;; esac\n'
    )
    # The doctor fixture in the pack passes only PATH and HOME to this real script.
    doctor = ROOT / "wecom/doctor/check-runtime.sh"
    (tools / "python3").write_text(
        '#!/bin/sh\n[ "$1" != -c ] || exit 0\n'
        f'exec env -i PATH="$PATH" HOME="$HOME" {shlex.quote(str(doctor))}\n'
    )
    result = run("verify -C wecom/tests python3 -m pytest -q .")
    assert result.returncode == 0, result.stderr + result.stdout
    assert "/curl " not in log.read_text()


def test_pytest_commands_leave_clean_tree(harness):
    repo, tools, _, _, run = harness
    _, blocks, _ = split_document(SKILL.read_text())
    pytest_blocks = [b for b in blocks if "python3 -m pytest " in b or b.startswith("verify make test-gascity")]
    for directory in re.search(r"^PYTEST_DIRS \?= (.+)$", (ROOT / "Makefile").read_text(), re.M)[1].split():
        (repo / directory).mkdir(parents=True, exist_ok=True)
    # Each invocation collects a real test, including every pack run from its own directory.
    for i, directory in enumerate([repo / "tests", *(repo / d for d in PYTHON_DIRS)]):
        (directory / f"test_fixture_{i}.py").write_text("def test_example():\n    assert True\n")
    (repo / "Makefile").write_bytes((ROOT / "Makefile").read_bytes())
    (repo / ".gitignore").write_text("__pycache__/\n")
    (tools / "python3").write_text(f'#!/bin/sh\nexec {shlex.quote(sys.executable)} "$@"\n')
    (tools / "make").write_text('#!/bin/sh\nexec /usr/bin/make "$@"\n')
    git = ["git", "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid"]
    subprocess.run([*git, "add", "."], cwd=repo, env=BOOT, check=True)
    subprocess.run([*git, "commit", "-qm", "fixture"], cwd=repo, env=BOOT, check=True)
    for block in pytest_blocks:
        result = run(block)
        assert result.returncode == 0 and "passed" in result.stdout, (block, result.stderr, result.stdout)
        status = subprocess.check_output([*git, "status", "--porcelain", "--untracked-files=all"], cwd=repo, env=BOOT)
        assert not status and not list(repo.rglob(".pytest_cache")), f"Checkout changed after {block}"


@pytest.mark.parametrize("filename", SECRETS)
def test_guard_refuses_before_dispatch(harness, filename):
    repo, _, _, _, run = harness
    (repo / ".gitignore").write_text("*\n")
    path = repo / filename
    path.parent.mkdir(parents=True, exist_ok=True)
    path.touch()
    result = run("verify touch started")
    assert result.returncode != 0 and "credential" in result.stderr and not (repo / "started").exists()


@pytest.mark.parametrize("target,expected", [
    ("test", ["node --test", "python3 -m pytest"]),
    ("test-reachable", ["node --test"]),
    ("test-gascity", ["python3 -m pytest"]),
    ("registry-format-validate", ["python3 validate_registry.py"]),
])
def test_make_runs_its_checks_and_reports_failure(harness, target, expected):
    repo, _, _, log, run = harness
    (repo / "Makefile").write_bytes((ROOT / "Makefile").read_bytes())
    # The real Make evaluates the recipes. Node, Python, and any fallback
    # package manager resolve to local stand-ins in the temporary checkout.
    result = run(f"verify /usr/bin/make {target}")
    assert result.returncode != 0
    calls = log.read_text()
    assert all(call in calls for call in expected), calls
    assert "/uv " not in calls


def test_each_block_returns_first_failure(harness):
    repo, tools, _, log, run = harness
    text = SKILL.read_text()
    check_inventory(text, (ROOT / "Makefile").read_text())
    _, blocks, _ = split_document(text)
    for block in blocks:
        log.with_suffix(".dispatched").unlink(missing_ok=True)
        if "vitest" in block:
            target = repo / "whatsapp/gateway/node_modules/.bin/vitest"
            target.parent.mkdir(parents=True, exist_ok=True)
            target.symlink_to(tools / "make")
        result = run(block)
        assert result.returncode == 17, (block, result.returncode, result.stderr)


@pytest.mark.parametrize("kind", ["private", "certificate", "read-error", "deep", "symlink", "fifo"])
def test_pem_boundary(harness, kind):
    repo, tools, _, _, run = harness
    path = repo / "fixture.pem"
    certificate = b"-----BEGIN CERTIFICATE-----\n"
    private = b"-----BEGIN PRIVATE KEY-----\n"
    if kind == "symlink":
        path.symlink_to(repo / "absent")
    elif kind == "fifo":
        os.mkfifo(path)
    else:
        path.write_bytes({"deep": certificate + b"x\n" * 31000 + private,
                          "private": private, "certificate": certificate, "read-error": certificate}[kind])
    if kind == "read-error":
        head = tools / "head"
        head.write_text("#!/bin/sh\nexit 1\n")
        head.chmod(0o755)
    result = run("verify touch started")
    assert (result.returncode == 0) == (kind == "certificate"), (kind, result.stderr)
    assert (repo / "started").exists() == (kind == "certificate")
    assert "-----BEGIN" not in result.stderr + result.stdout


@pytest.mark.parametrize(("name", "check", "need"), [
    ("python3", "make test-gascity", "pytest"), ("node", "make test-reachable", "Node"),
    ("jq", "gastown", "jq"), ("rg", "gastown", "rg"), ("bash", "gastown", "Bash"),
    ("sed", "gastown", "GNU sed"), ("mktemp", "gastown", "GNU mktemp"), ("date", "gastown", "GNU date"),
])
def test_missing_prerequisite_never_dispatches(harness, name, check, need):
    _, tools, _, log, run = harness
    tool = tools / name
    tool.write_text('#!/bin/sh\nif [ "$1" = --version ]; then exit 1; fi\nexec /usr/bin/mktemp "$@"\n'
                    if name == "mktemp" else "#!/bin/sh\nexit 1\n")
    tool.chmod(0o755)
    _, blocks, _ = split_document(SKILL.read_text())
    line = next(b for b in blocks if "gastown/tests/test_" in b) if check == "gastown" else "verify " + check
    result = run(line)
    assert result.returncode != 0 and need in result.stderr, (line, result.stderr)
    calls = log.read_text() if log.exists() else ""
    assert not re.search(r"/(?:make|curl|brew|uv|npm|pnpm) ", calls)
    assert "bash -ec" not in calls


@pytest.mark.parametrize("missing", ["pytest", "yaml", "jsonschema"])
def test_make_requires_each_python_import(harness, missing):
    repo, tools, _, log, run = harness
    for module in {"pytest", "yaml", "jsonschema"} - {missing}:
        (repo / f"{module}.py").touch()
    (tools / "python3").write_text(f'#!/bin/sh\nexec {shlex.quote(sys.executable)} -S "$@"\n')
    result = run("verify make test-gascity")
    assert result.returncode == 2 and "Missing" in result.stderr
    assert "/make " not in log.read_text()

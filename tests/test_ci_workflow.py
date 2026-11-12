from pathlib import Path
import shlex
from collections import defaultdict

import yaml


REPO_ROOT = Path(__file__).resolve().parents[1]


def test_python_suite_directories_match_and_modules_have_unique_names() -> None:
    workflow = yaml.safe_load((REPO_ROOT / ".github/workflows/ci.yml").read_text())
    step = next(s for s in workflow["jobs"]["check"]["steps"]
                if s.get("name") == "Run Python pack tests")
    ci_dirs = shlex.split(step["run"])[3:]
    ci_dirs = [d for d in ci_dirs if not d.startswith("-")]
    make_line = next(line for line in (REPO_ROOT / "Makefile").read_text().splitlines()
                     if line.startswith("PYTEST_DIRS ?="))
    make_dirs = shlex.split(make_line.split("?=", 1)[1])
    problems = []
    if set(ci_dirs) != set(make_dirs):
        problems.append(f"CI/Makefile directory mismatch: {set(ci_dirs) ^ set(make_dirs)}")
    names = defaultdict(list)
    for directory in sorted(set(ci_dirs + make_dirs)):
        assert (REPO_ROOT / directory).is_dir(), directory
        for module in (REPO_ROOT / directory).rglob("test_*.py"):
            names[module.stem].append(str(module.relative_to(REPO_ROOT)))
    problems.extend(f"duplicate module {name}: {paths}" for name, paths in names.items()
                    if len(paths) > 1)
    assert not problems, "\n".join(problems)


def test_order_scope_regression_runs_with_installed_gc() -> None:
    workflow = yaml.safe_load(
        REPO_ROOT.joinpath(".github", "workflows", "ci.yml").read_text(encoding="utf-8")
    )
    jobs = workflow["jobs"]
    assert "check" in jobs, f"no job 'check' in CI; found: {sorted(jobs)}"
    steps = jobs["check"]["steps"]
    step_name = "Lint and exercise shared role prompt composition"
    matching = [step for step in steps if step.get("name") == step_name]
    assert matching, (
        f"no step named {step_name!r} in CI job 'check'; "
        f"found: {[step.get('name') for step in steps]}"
    )

    invocation = (
        'GC_TEST_BIN="$GC_BIN" python3 -m pytest '
        "oversight-rig/tests/test_order_scopes.py -q"
    )
    # Look at the step's *active* lines, not the raw block: a commented-out or
    # `|| true`-suffixed invocation still contains the substring while no
    # longer running, or no longer able to fail the job.
    active = [
        line.strip()
        for line in matching[0]["run"].splitlines()
        if line.strip() and not line.strip().startswith("#")
    ]
    running = [line for line in active if line.startswith(invocation)]
    assert running, (
        f"{step_name!r} does not run the order-scope regression "
        f"(absent or commented out); active lines: {active}"
    )
    assert not any("||" in line for line in running), (
        "the order-scope regression is short-circuited, so a failure cannot "
        f"fail the job: {running}"
    )

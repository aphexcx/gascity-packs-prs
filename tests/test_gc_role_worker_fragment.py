"""Static checks on the shared `gc-role-worker` template fragment.

The rendered-prompt integration tests need a real gc binary (GC_TEST_BIN);
these run everywhere and pin the fragment text every role worker inherits.
"""

from __future__ import annotations

from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parents[1]
FRAGMENT = REPO_ROOT / "gascity" / "template-fragments" / "gc-role-worker.template.md"


def fragment() -> str:
    return FRAGMENT.read_text(encoding="utf-8")


def test_fragment_defines_exactly_one_template() -> None:
    text = fragment()
    assert text.count('{{ define "gc-role-worker" -}}') == 1
    assert text.count("{{- end }}") == 1
    assert text.count("# GC Role Worker") == 1


def test_claim_section_tolerates_omitted_lifecycle_keys() -> None:
    text = fragment()
    claim = text.split("## Claim", 1)[1].split("## Workspace", 1)[0]
    assert "CLAIMED_ROOT_BEAD_ID" in claim
    assert "CLAIMED_CONTINUATION_GROUP" in claim
    assert "An absent key is an empty value, never a failed claim." in claim


def test_workspace_section_sits_between_claim_and_close() -> None:
    text = fragment()
    claim_at = text.index("## Claim")
    workspace_at = text.index("## Workspace")
    close_at = text.index("## Close")
    assert claim_at < workspace_at < close_at
    workspace = text[workspace_at:close_at]
    assert "$GC_DIR" in workspace
    assert "worker-worktree.sh" in workspace
    assert "if `git branch --show-current` prints" in workspace
    assert "restamp it when they differ" in workspace
    assert "--set-metadata 'work_dir=<absolute worktree path>'" in workspace
    assert "--set-metadata 'gc.work_branch=<branch>'" in workspace


def test_workspace_section_never_names_the_rig_root_as_a_place_to_work() -> None:
    workspace = fragment().split("## Workspace", 1)[1].split("## Close", 1)[0]
    assert "never work in the rig root" in workspace
    assert "the rig root is a human\ncheckout" in workspace
    assert "cd " not in workspace


HUNTING_RULES = (
    ".worktrees/<rig>/<bead>",
    "create your\nown worktree",
    "create your own worktree",
    "If you start in the rig root",
)


def test_workers_never_choose_or_create_their_workspace() -> None:
    """gc chooses the workspace (agent work_dir + pre_start); no role prompt
    tells a worker to go and make its own worktree."""
    workspace = fragment().split("## Workspace", 1)[1].split("## Close", 1)[0]
    assert "You never pick, create, or hunt for\na workspace" in workspace
    prompts = [FRAGMENT, *sorted((REPO_ROOT / "gascity" / "roles" / "agents").glob("*/prompt.template.md"))]
    assert len(prompts) > 1
    for path in prompts:
        text = path.read_text(encoding="utf-8")
        for rule in HUNTING_RULES:
            assert rule not in text, f"{path.relative_to(REPO_ROOT)} still tells the worker to hunt for a worktree: {rule!r}"


def test_worker_worktree_script_is_shipped_and_executable() -> None:
    script = REPO_ROOT / "gascity" / "assets" / "scripts" / "worker-worktree.sh"
    assert script.is_file()
    assert script.stat().st_mode & 0o111, "worker-worktree.sh must be executable"
    head = script.read_text(encoding="utf-8").splitlines()[0]
    assert head == "#!/bin/sh"

"""Static checks on the shared `gc-role-worker` template fragment.

The rendered-prompt integration tests need a real gc binary (GC_TEST_BIN);
these run everywhere and pin the fragment text every role worker inherits.
"""

from __future__ import annotations

import re
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

FALLBACK_OPENER = "If `$GC_DIR` is the rig root"


def paragraphs(text: str) -> list[str]:
    """Blank-line separated paragraphs of a prompt, whitespace-stripped."""
    return [para.strip() for para in re.split(r"\n[ \t]*\n", text) if para.strip()]


def workspace_section() -> str:
    return fragment().split("## Workspace", 1)[1].split("## Close", 1)[0]


def test_workers_never_choose_or_create_their_workspace() -> None:
    """gc chooses the workspace (agent work_dir + pre_start). The PRIMARY text
    of every role prompt never tells a worker to go and make its own worktree;
    the hunting phrases may appear only inside the one conditional fallback
    paragraph (see test_workspace_fallback_covers_every_unconfigured_role)."""
    workspace = workspace_section()
    assert "You never pick, create, or hunt for\na workspace" in workspace
    prompts = [FRAGMENT, *sorted((REPO_ROOT / "gascity" / "roles" / "agents").glob("*/prompt.template.md"))]
    assert len(prompts) > 1
    for path in prompts:
        for para in paragraphs(path.read_text(encoding="utf-8")):
            if para.startswith(FALLBACK_OPENER):
                continue
            for rule in HUNTING_RULES:
                assert rule not in para, (
                    f"{path.relative_to(REPO_ROOT)} tells the worker to hunt for a worktree "
                    f"outside the conditional fallback: {rule!r} in {para[:60]!r}"
                )


def test_workspace_fallback_covers_every_unconfigured_role() -> None:
    """One fallback paragraph for a city that gave the role no work_dir. It is
    conditional ONLY on $GC_DIR being the rig root: the pack ships every role
    without work_dir/pre_start, so in a default installation a role starts in
    the rig root, is told never to work there, and needs one permitted place
    (gate r3 M2). Round 2 keyed the fallback on the rig's rules forbidding
    root work, which left a rig without such a rule no workspace at all; that
    condition is gone. The paragraph sits after the primary text and tells the
    worker to mail the mayor for a lane."""
    workspace = workspace_section()
    fallbacks = [para for para in paragraphs(workspace) if para.startswith(FALLBACK_OPENER)]
    assert len(fallbacks) == 1, "exactly one conditional fallback paragraph"
    fallback = fallbacks[0]
    assert workspace.index("You never pick, create, or hunt for") < workspace.index(FALLBACK_OPENER)
    # The primary text still describes the configured case.
    assert "never work in the rig root" in workspace
    flat = " ".join(fallback.split())
    for clause in (
        "If `$GC_DIR` is the rig root, this role has no `work_dir` in your city: create your worktree under",
        "`<city>/.worktrees/<rig>/<bead>` (check out the bead's branch if it already exists)",
        "work there, and mail the mayor that this role needs a lane",
        "(`work_dir` + `pre_start`, see README, Worker workspaces)",
    ):
        assert clause in flat, f"fallback paragraph lacks: {clause!r}"
    # The only condition is the rig root; nothing in the Workspace section keys
    # the fallback on the rig's rules.
    for gone in ("When the rig's rules forbid", "rules forbid", "forbid working there"):
        assert gone not in " ".join(workspace.split()), f"round-2 condition still present: {gone!r}"
    # The fallback never fires where lanes are configured: no other paragraph
    # of the Workspace section is conditional on the rig root.
    assert sum(para.startswith("If ") for para in paragraphs(workspace)) == 1


SCRIPT = REPO_ROOT / "gascity" / "assets" / "scripts" / "worker-worktree.sh"
WORKFLOWS = REPO_ROOT / "gascity" / "assets" / "workflows"
FORMULAS = REPO_ROOT / "gascity" / "formulas"


def script_base_order() -> list[str]:
    """The `--base` resolution order worker-worktree.sh implements, read from
    the script itself so the prompt cannot drift from it: the remote refs it
    probes, in source order, then its literal HEAD fallback."""
    block = SCRIPT.read_text(encoding="utf-8").split('if [ -z "$BASE" ]; then', 1)[1].split("BASE_SHA=", 1)[0]
    probes = re.findall(r"refs/remotes/\$REMOTE/(HEAD|main|master)", block)
    assert 'BASE="HEAD"' in block, "the script no longer falls back to the rig's HEAD"
    return [f"<remote>/{name}" for name in probes] + ["HEAD"]


def test_workspace_fallback_resolves_its_base_and_needs_no_origin_remote() -> None:
    """Gate r7 (round 8): the fallback required `origin/<default branch>`, so a
    local-only rig, or one whose remote is not named origin, could never get a
    workspace under a paragraph that now covers every unconfigured role. The
    fallback RESOLVES the base in worker-worktree.sh's order and ends at the
    rig checkout's HEAD; the order is read from the script, not retyped."""
    fallback = [para for para in paragraphs(workspace_section()) if para.startswith(FALLBACK_OPENER)][0]
    flat = " ".join(fallback.split())
    order = script_base_order()
    assert order == ["<remote>/HEAD", "<remote>/main", "<remote>/master", "HEAD"]
    # Every candidate is named, in the script's order, after the sentence
    # that says the base is resolved; the last one is the rig's own HEAD.
    resolve_at = flat.index("Resolve the base of a new branch, never assume it")
    positions = [flat.index(f"`{ref}`", resolve_at) for ref in order[:-1]]
    positions.append(flat.index("else the rig checkout's current `HEAD`", resolve_at))
    assert positions == sorted(positions), flat
    for clause in (
        "in the order `worker-worktree.sh` uses",
        "the remote's default branch when the rig has a remote",
        "when the remote has another name, that name from `git -C <rig root> remote`",
        "Pin the base to a commit in the rig",
        "create the worktree from that commit",
        "This fallback never fails for lack of a remote",
        "never writes into the rig checkout",
        "the checkout's HEAD and files do not move",
    ):
        assert clause in flat, f"fallback paragraph lacks: {clause!r}"
    # No bare origin requirement anywhere a worker reads: the fragment, every
    # role prompt, every workflow step, every formula (the sweep, pinned).
    consumers = [
        FRAGMENT,
        *sorted((REPO_ROOT / "gascity" / "roles" / "agents").glob("*/prompt.template.md")),
        *sorted(WORKFLOWS.rglob("*.md")),
        *sorted(FORMULAS.glob("*.toml")),
    ]
    assert len(consumers) > 20
    for path in consumers:
        text = path.read_text(encoding="utf-8")
        for bare in ("origin/", "<default branch>"):
            assert bare not in text, f"{path.relative_to(REPO_ROOT)} assumes a remote named origin: {bare!r}"


def test_worker_worktree_script_is_shipped_and_executable() -> None:
    script = REPO_ROOT / "gascity" / "assets" / "scripts" / "worker-worktree.sh"
    assert script.is_file()
    assert script.stat().st_mode & 0o111, "worker-worktree.sh must be executable"
    head = script.read_text(encoding="utf-8").splitlines()[0]
    assert head == "#!/bin/sh"

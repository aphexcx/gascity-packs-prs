from __future__ import annotations

from dataclasses import dataclass
import json
import os
from pathlib import Path
import shlex
import subprocess
import sys
import textwrap

import pytest


REPO_ROOT = Path(__file__).resolve().parents[1]
GASCITY_CORE_SOURCE = (
    "https://github.com/gastownhall/gascity.git//internal/bootstrap/packs/core"
)


@dataclass(frozen=True)
class PromptWorkspace:
    city_dir: Path
    rig_dir: Path
    env: dict[str, str]


@pytest.fixture(scope="session")
def gc_test_bin() -> Path:
    configured = os.environ.get("GC_TEST_BIN")
    if not configured:
        pytest.skip("set GC_TEST_BIN to run real Gas City CLI integration tests")

    binary = Path(configured).expanduser().resolve()
    if not binary.is_file() or not os.access(binary, os.X_OK):
        pytest.fail(f"GC_TEST_BIN is not an executable file: {binary}")
    return binary


def _write_workspace_scaffold(
    root: Path,
    *,
    workspace_name: str,
    pack_toml: str,
    city_toml: str,
    extra_env: dict[str, str] | None = None,
) -> PromptWorkspace:
    """Build the hermetic fixture city every integration fixture here shares.

    Fixtures differ only in their pack.toml/city.toml contents and in env
    deltas; the directory layout, the site.toml rig registration, and the
    isolating env base are identical, so they live here once.
    """
    city_dir = root / "city"
    rig_dir = root / "fixture"
    gc_home = root / "gc-home"
    home = root / "home"
    (city_dir / ".gc").mkdir(parents=True)
    rig_dir.mkdir()
    gc_home.mkdir()
    home.mkdir()

    city_dir.joinpath("pack.toml").write_text(pack_toml, encoding="utf-8")
    city_dir.joinpath("city.toml").write_text(city_toml, encoding="utf-8")
    city_dir.joinpath(".gc", "site.toml").write_text(
        textwrap.dedent(
            f"""\
            workspace_name = {json.dumps(workspace_name)}

            [[rig]]
            name = "fixture"
            path = {json.dumps(str(rig_dir))}
            """
        ),
        encoding="utf-8",
    )

    env = {
        **os.environ,
        "HOME": str(home),
        "GC_HOME": str(gc_home),
        "GC_CITY": str(city_dir),
        "GC_CITY_PATH": str(city_dir),
        "GC_CITY_ROOT": str(city_dir),
        "GC_RIG": "fixture",
        **(extra_env or {}),
    }
    return PromptWorkspace(city_dir=city_dir, rig_dir=rig_dir, env=env)


def write_prompt_workspace(
    root: Path,
    *,
    city_binding: str,
    city_pack: Path,
) -> PromptWorkspace:
    roles_source = (REPO_ROOT / "gascity" / "roles").resolve()
    city_pack = city_pack.resolve()
    return _write_workspace_scaffold(
        root,
        workspace_name="prompt-integration",
        pack_toml=textwrap.dedent(
            f"""\
            [pack]
            name = "prompt-integration"
            schema = 2

            [imports.{city_binding}]
            source = {json.dumps(str(city_pack))}
            """
        ),
        city_toml=textwrap.dedent(
            f"""\
            [workspace]
            provider = "codex"

            [providers.codex]
            base = "builtin:codex"

            [[rigs]]
            name = "fixture"

            [rigs.imports.gc]
            source = {json.dumps(str(roles_source))}
            """
        ),
    )


def write_gastown_sling_workspace(root: Path) -> PromptWorkspace:
    gastown_source = (REPO_ROOT / "gastown").resolve()
    return _write_workspace_scaffold(
        root,
        workspace_name="gastown-sling-integration",
        pack_toml=textwrap.dedent(
            f"""\
            [pack]
            name = "gastown-sling-integration"
            schema = 2

            [imports.core]
            source = {json.dumps(GASCITY_CORE_SOURCE)}
            """
        ),
        city_toml=textwrap.dedent(
            f"""\
            [workspace]
            provider = "opencode"

            [providers.opencode]
            base = "builtin:opencode"

            [beads]
            provider = "file"

            [[rigs]]
            name = "fixture"
            prefix = "gc"
            default_branch = "main"

            [rigs.imports.gastown]
            source = {json.dumps(str(gastown_source))}
            """
        ),
        extra_env={"GC_BEADS": "file"},
    )


def run_gc(
    gc_test_bin: Path,
    workspace: PromptWorkspace,
    *args: str,
    cwd: Path | None = None,
    env: dict[str, str] | None = None,
) -> subprocess.CompletedProcess[str]:
    return subprocess.run(
        [str(gc_test_bin), *args],
        cwd=cwd or workspace.rig_dir,
        env=env or workspace.env,
        text=True,
        capture_output=True,
        timeout=30,
    )


def discover_provenance_binding(
    gc_test_bin: Path, workspace: PromptWorkspace, prompt: str, tmp_path: Path,
) -> subprocess.CompletedProcess[str]:
    """Run the rendered close discovery with real gc, without touching a store."""
    script = "PROVENANCE_PACK_ALIAS=" + prompt.split(
        "```bash\nPROVENANCE_PACK_ALIAS=", 1
    )[1].split("```", 1)[0]
    binary_dir = tmp_path / "real-gc-bin"
    binary_dir.mkdir()
    (binary_dir / "gc").symlink_to(gc_test_bin)
    return subprocess.run(
        ["sh", "-c", script],
        cwd=workspace.rig_dir,
        env={**workspace.env, "PATH": str(binary_dir) + os.pathsep + os.environ["PATH"]},
        text=True, capture_output=True, timeout=30,
    )


def assert_clean_worker_render(prompt: str, persona_heading: str) -> None:
    assert persona_heading in prompt
    assert prompt.count("# GC Role Worker") == 1
    assert '{{ template "gc-role-worker" . }}' not in prompt
    assert "gc hook --claim --drain-ack --json" in prompt
    assert "gc gc claim" not in prompt
    assert "CLAIMED_BEAD_ID" in prompt
    assert "CLAIMED_ROOT_BEAD_ID" in prompt
    assert "CLAIMED_CONTINUATION_GROUP" in prompt
    assert 'gc bd update "$CLAIMED_BEAD_ID"' in prompt
    assert "An absent key is an empty value, never a failed claim." in prompt
    assert prompt.count("## Workspace") == 1
    assert "--set-metadata 'gc.outcome=pass'" in prompt
    assert "--set-metadata 'gc.work_outcome=shipped'" in prompt
    assert 'gc.work_commit=$WORK_COMMIT' in prompt
    assert 'gc.work_branch=$WORK_BRANCH' in prompt
    assert "Refusing to write an empty gc.work_branch." in prompt
    assert "An empty continuation group is a hard session boundary" in prompt
    assert "Never merge a pull request yourself unless the bead explicitly" in prompt
    assert "`gc.failure_class=pr_handoff_failed`" in prompt
    assert "gc mail send mayor" not in prompt
    assert '"$GC_CITY_PATH/assets/ops/mayor-tools/codex-gate.sh" review' in prompt
    assert "codex -p city -m gpt-6-astra exec --skip-git-repo-check" in prompt


def test_city_scoped_gascity_registers_claim_command_from_rig(
    tmp_path: Path, gc_test_bin: Path
) -> None:
    workspace = write_prompt_workspace(
        tmp_path,
        city_binding="gc",
        city_pack=REPO_ROOT / "gascity",
    )

    result = run_gc(gc_test_bin, workspace, "gc", "claim", "--help")

    assert result.returncode == 0, result.stderr
    assert "Atomically claim one routed workflow bead" in result.stdout
    assert "Usage:\n  gc gc claim" in result.stdout
    assert "Gas City CLI — orchestration-builder" not in result.stdout


def test_plain_gastown_polecat_sling_starts_default_graph_workflow(
    tmp_path: Path, gc_test_bin: Path
) -> None:
    workspace = write_gastown_sling_workspace(tmp_path)

    result = run_gc(
        gc_test_bin,
        workspace,
        "--city",
        str(workspace.city_dir),
        "--rig",
        "fixture",
        "sling",
        "fixture/gastown.polecat",
        "Exercise the default polecat workflow",
        "--json",
    )

    assert result.returncode == 0, result.stderr
    payload = json.loads(result.stdout)
    assert payload["target"] == "fixture/gastown.polecat"
    assert payload["method"] == "default-on-formula"
    assert payload["formula"] == "mol-polecat-work"
    assert payload["workflow_id"]
    assert payload["bead_id"] == payload["workflow_id"]
    assert "molecule_id" not in payload


@pytest.mark.parametrize(
    ("city_binding", "pack_path", "agent", "persona_heading"),
    (
        ("gc", "gascity", "gc.implementation-worker", "# GC Role Worker"),
        ("gascity", "gascity", "gc.implementation-worker", "# GC Role Worker"),
        ("custom", "gascity", "gc.implementation-worker", "# GC Role Worker"),
        ("bmad", "bmad", "bmad.story-implementer", "# BMAD Story Implementer"),
        (
            "superpowers",
            "superpowers",
            "superpowers.implementer",
            "# Superpowers Implementer",
        ),
        ("gstack", "gstack", "gstack.implementer", "# gstack Implementer"),
        (
            "compound-engineering",
            "compound-engineering",
            "compound-engineering.ce-work",
            "# Compound Engineering Worker",
        ),
        ("profiler", "profiler", "profiler.profile-analyst", "# Profile Analyst"),
    ),
)
def test_role_prompts_render_public_worker_fragment(
    tmp_path: Path,
    gc_test_bin: Path,
    city_binding: str,
    pack_path: str,
    agent: str,
    persona_heading: str,
) -> None:
    workspace = write_prompt_workspace(
        tmp_path,
        city_binding=city_binding,
        city_pack=REPO_ROOT / pack_path,
    )

    result = run_gc(
        gc_test_bin,
        workspace,
        "--city",
        str(workspace.city_dir),
        "prime",
        "--strict",
        f"fixture/{agent}",
        cwd=workspace.city_dir,
    )

    assert result.returncode == 0, result.stderr
    assert_clean_worker_render(result.stdout, persona_heading)

    # The city command binding may differ from the rig role binding. Exercise
    # the command actually rendered, without claiming work in the fixture.
    claim = result.stdout.split("```bash\n", 1)[1].split("\n", 1)[0]
    command = shlex.split(claim)
    assert command == ["gc", "hook", "--claim", "--drain-ack", "--json"]
    help_result = run_gc(gc_test_bin, workspace, *command[1:], "--help")
    assert help_result.returncode == 0, help_result.stderr
    assert "--claim" in help_result.stdout
    assert "--drain-ack" in help_result.stdout

    discovery = discover_provenance_binding(gc_test_bin, workspace, result.stdout, tmp_path)
    assert discovery.returncode == 0, discovery.stderr
    expected_binding = city_binding if pack_path == "gascity" else "gc"
    assert discovery.stdout.strip() == f"Provenance binding: {expected_binding}"
    provenance_help = run_gc(gc_test_bin, workspace, expected_binding, "provenance", "--help")
    assert provenance_help.returncode == 0, provenance_help.stderr
    assert "Record or summarize a normal worker's typed close trail" in provenance_help.stdout


def test_worker_provenance_discovery_fails_without_city_command_import(
    tmp_path: Path, gc_test_bin: Path,
) -> None:
    workspace = write_prompt_workspace(
        tmp_path, city_binding="gc", city_pack=REPO_ROOT / "gascity",
    )
    # Keep the rig roles while removing the city command import entirely.
    (workspace.city_dir / "pack.toml").write_text(
        '[pack]\nname = "no-command-import"\nschema = 2\n', encoding="utf-8",
    )
    prompt = run_gc(gc_test_bin, workspace, "prime", "--strict", "fixture/gc.implementation-worker")
    assert prompt.returncode == 0, prompt.stderr
    discovery = discover_provenance_binding(gc_test_bin, workspace, prompt.stdout, tmp_path)
    assert discovery.returncode != 0
    assert "No gc binding serves provenance: check gc import list and the Gas City pack pin." in discovery.stderr
    assert not discovery.stdout


def test_registered_claim_command_dispatches_store_aware_show_and_normalizes_json(
    tmp_path: Path, gc_test_bin: Path
) -> None:
    workspace = write_prompt_workspace(
        tmp_path,
        city_binding="gc",
        city_pack=REPO_ROOT / "gascity",
    )
    fake_bin = tmp_path / "fake-bin"
    fake_bin.mkdir()
    calls = tmp_path / "nested-gc-calls"
    nested_gc = fake_bin / "gc"
    nested_gc.write_text(
        textwrap.dedent(
            """\
            #!/bin/sh
            printf '%s\n' "$*" >>"$GC_TEST_CALLS"
            if [ "$1" = hook ] && [ "$2" = --claim ] && [ "$3" = --drain-ack ] && [ "$4" = --json ]; then
              printf '%s\n' '{"action":"work","bead_id":"bd-123","assignee":"worker","route":"gc.implementation-worker"}'
            elif [ "$1" = b""d ] && [ "$2" = show ] && [ "$3" = bd-123 ] && [ "$4" = --json ]; then
              printf '%s\n' '{"id":"bd-123","status":"in_progress","assignee":"worker","metadata":{"gc.routed_to":"gc.implementation-worker","gc.root_bead_id":"root-1","gc.continuation_group":"group-1"}}'
            else
              printf 'unexpected nested gc invocation: %s\n' "$*" >&2
              exit 2
            fi
            """
        ),
        encoding="utf-8",
    )
    nested_gc.chmod(0o755)
    env = {
        **workspace.env,
        "BEADS_ACTOR": "worker",
        "GC_AGENT": "gc.implementation-worker",
        "GC_TEST_CALLS": str(calls),
        "PATH": f"{fake_bin}:/usr/bin:/bin",
    }

    result = run_gc(gc_test_bin, workspace, "gc", "claim", env=env)

    assert result.returncode == 0, result.stderr
    payload = json.loads(result.stdout)
    assert payload["bead_id"] == "bd-123"
    assert payload["root_bead_id"] == "root-1"
    assert payload["continuation_group"] == "group-1"
    assert payload["bead"]["id"] == "bd-123"
    assert calls.read_text(encoding="utf-8").splitlines() == [
        "hook --claim --drain-ack --json",
        " ".join(("b" + "d", "show", "bd-123", "--json")),
    ]


@pytest.mark.parametrize('binding', ['gc', 'gascity', 'custom'])
def test_provenance_command_is_available_under_city_import_alias(tmp_path: Path, gc_test_bin: Path, binding: str) -> None:
    workspace = write_prompt_workspace(tmp_path, city_binding=binding, city_pack=REPO_ROOT / 'gascity')
    help_result = run_gc(gc_test_bin, workspace, binding, 'provenance', '--help')
    assert help_result.returncode == 0, help_result.stderr
    assert "Record or summarize a normal worker's typed close trail" in help_result.stdout
    result = run_gc(gc_test_bin, workspace, binding, 'provenance', 'record', 'fixture-1', '--outcome', 'shipped', '--commit', 'invalid')
    assert result.returncode != 0
    assert 'commit must be a full' in result.stderr


@pytest.mark.parametrize(('binding', 'pack_path'), [
    ('gc', 'gascity'), ('gascity', 'gascity'), ('custom', 'gascity'),
    ('bmad', 'bmad'), ('superpowers', 'superpowers'), ('gstack', 'gstack'),
    ('compound-engineering', 'compound-engineering'), ('profiler', 'profiler'),
])
@pytest.mark.parametrize('scope_form', [
    'environment', 'post-city', 'post-city-equals', 'post-rig',
    'post-rig-equals', 'pre-city', 'pre-city-rig', 'post-city-rig',
])
def test_provenance_record_and_summary_preserve_explicit_scope(
    tmp_path: Path, gc_test_bin: Path, binding: str, pack_path: str, scope_form: str,
) -> None:
    """Real pack dispatch AND nested gc routing; only the bd store is a fake."""
    selected_root = tmp_path / 'selected'
    selected_root.mkdir()
    workspace = write_prompt_workspace(
        selected_root, city_binding=binding, city_pack=REPO_ROOT / pack_path,
    )
    other_rig = selected_root / 'other'
    other_rig.mkdir()
    with (workspace.city_dir / 'city.toml').open('a') as config:
        config.write('\n[[rigs]]\nname = "other"\n')
    with (workspace.city_dir / '.gc/site.toml').open('a') as site:
        site.write('\n[[rig]]\nname = "other"\npath = ' + json.dumps(str(other_rig)) + '\n')
    for scope in (workspace.city_dir, workspace.rig_dir, other_rig):
        (scope / '.beads').mkdir()

    ambient = workspace
    if 'city' in scope_form:
        ambient_root = tmp_path / 'ambient'
        ambient_root.mkdir()
        ambient = write_prompt_workspace(
            ambient_root, city_binding=binding, city_pack=REPO_ROOT / pack_path,
        )

    binary_dir = tmp_path / 'bin'
    binary_dir.mkdir()
    (binary_dir / 'gc').symlink_to(gc_test_bin)
    store = binary_dir / ('b' + 'd')
    state = tmp_path / 'store.json'
    state.write_text(json.dumps({'calls': [], 'events': []}))
    store.write_text(f'#!{sys.executable}\n' + textwrap.dedent('''\
        import json, os, sys
        from pathlib import Path
        path = Path(os.environ['PROVENANCE_TEST_STATE'])
        state = json.loads(path.read_text())
        args = sys.argv[1:]
        state['calls'].append({'args': args, 'cwd': str(Path.cwd()),
                               'city': os.environ.get('GC_CITY'),
                               'city_path': os.environ.get('GC_CITY_PATH'),
                               'city_name': os.environ.get('GC_CITY_NAME')})
        if args == ['show', 'work-1', '--json']:
            print('[{"id":"work-1"}]')
        elif args == ['provenance', 'log', 'work-1', '--json']:
            print(json.dumps(state['events']))
        elif args[:2] == ['provenance', 'record']:
            options = dict(zip(args[2::2], args[3::2]))
            assert options['--issue'] == 'work-1'
            event = {key: options['--' + key.replace('_', '-')]
                     for key in ['kind', 'source', 'ref', 'ref_kind', 'payload']}
            event.update(id=str(len(state['events'])), occurred_at=options['--at'])
            state['events'].append(event)
        elif args[:2] != ['update', 'work-1']:
            sys.exit('unexpected store argv: ' + repr(args))
        path.write_text(json.dumps(state))
    '''))
    store.chmod(0o755)
    # Do not inherit live worker store/auth/API routing. Every store process
    # must be this fixture executable, including recursive gc calls.
    env = {key: value for key, value in ambient.env.items()
           if not key.startswith(('GC_', 'BEADS_', 'BD_'))}
    env.update({
        'GC_HOME': ambient.env['GC_HOME'], 'GC_CITY': str(ambient.city_dir),
        'GC_CITY_PATH': str(ambient.city_dir), 'GC_CITY_ROOT': str(ambient.city_dir),
        'GC_RIG': 'other', 'GC_BEADS': 'bd', 'BD_BIN': str(store),
        'GC_BIN': str(gc_test_bin), 'GC_NO_API': '1',
        'PATH': str(binary_dir) + os.pathsep + os.environ['PATH'],
        'PROVENANCE_TEST_STATE': str(state),
    })
    city_flags = ['--city', str(workspace.city_dir)]
    command_binding = binding if pack_path == 'gascity' else 'gc'
    prefix = [command_binding, 'provenance']
    if scope_form == 'post-city':
        prefix += city_flags
    elif scope_form == 'post-city-equals':
        prefix += ['--city=' + str(workspace.city_dir)]
    elif scope_form == 'post-rig':
        prefix += ['--rig', 'fixture']
    elif scope_form == 'post-rig-equals':
        prefix += ['--rig=fixture']
    elif scope_form == 'pre-city':
        prefix = [command_binding, *city_flags, 'provenance']
    elif scope_form == 'pre-city-rig':
        prefix = [command_binding, *city_flags, '--rig', 'fixture', 'provenance']
    elif scope_form == 'post-city-rig':
        prefix += [*city_flags, '--rig=fixture']

    commit = 'a' * 40
    for args in (
        ['record', 'work-1', '--outcome', 'shipped', '--commit', commit,
         '--pr', 'https://github.com/owner/repo/pull/123'],
        ['summary', 'work-1'],
    ):
        result = run_gc(gc_test_bin, ambient, *prefix, *args, env=env)
        assert result.returncode == 0, result.stderr
    assert result.stdout.strip() == f'PR 123 at {commit}, gate unknown, read unknown'
    calls = json.loads(state.read_text())['calls']
    assert len(calls) == 8
    expected_scope = (workspace.rig_dir if 'rig' in scope_form else
                      workspace.city_dir if 'city' in scope_form else other_rig)
    assert all(Path(call['cwd']) == expected_scope.resolve() for call in calls)
    # PackRuntimeEnv pins these city anchors. Forwarding explicit flags also
    # preserves rig selection and explicit city-store (rather than rig) scope.
    assert all(Path(call['city']).resolve() == workspace.city_dir.resolve() for call in calls)
    assert all(Path(call['city_path']).resolve() == workspace.city_dir.resolve() for call in calls)
    assert all(call['city_name'] == 'prompt-integration' for call in calls)


@pytest.mark.parametrize('option', ['--city', '--rig', '--city=', '--rig='])
def test_provenance_explicit_scope_missing_value_is_named(
    tmp_path: Path, gc_test_bin: Path, option: str,
) -> None:
    workspace = write_prompt_workspace(
        tmp_path, city_binding='custom', city_pack=REPO_ROOT / 'gascity',
    )
    result = run_gc(gc_test_bin, workspace, 'custom', 'provenance', option)
    assert result.returncode == 2, result.stderr
    assert f'missing value for {option.rstrip("=")}' in result.stderr

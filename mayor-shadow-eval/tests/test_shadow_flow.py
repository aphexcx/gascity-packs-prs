"""Synthetic replay through a real subprocess + real hook; never calls a provider."""
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import sys
import time

import pytest

PACK = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(PACK / "scripts"))
import shadow_common as common
import shadow_watch as watch
import shadow_judge as judge


def row(kind, ident, content=None, **extra):
    return {"type": kind, "uuid": ident, "parentUuid": "parent-synthetic", "sessionId": "source-synthetic",
            "timestamp": "2026-10-27T12:00:00Z", "message": {"content": content or []}, **extra}


def conversation():
    return [
        row("user", "role", "# Mayor\n\nYou are the mayor of this Gas City workspace. Synthetic fixture."),
        row("system", "compact", subtype="compact_boundary", compactMetadata={"trigger": "auto"}),
        row("user", "summary", "Synthetic summary retained", isCompactSummary=True),
        row("attachment", "hook", attachment={"type": "hook_success", "content": "synthetic"}),
        row("user", "meta", "Synthetic hook output", isMeta=True),
        row("user", "trigger-synthetic", "Inspect the synthetic fixture and propose a file update."),
        row("assistant", "actual", [{"type": "tool_use", "id": "actual-tool", "name": "Write", "input": {"file_path": "synthetic-target", "content": "synthetic"}}]),
        row("user", "tool-result", [{"type": "tool_result", "tool_use_id": "actual-tool", "content": "done"}]),
        row("assistant", "actual-text", "Synthetic file updated."),
        row("user", "next", "Next synthetic trigger."),
    ]


FAKE_CLAUDE = r'''
import json,os,pathlib,subprocess,sys,time
a=sys.argv[1:]
root=pathlib.Path(os.environ['SHADOW_WORK_DIR'])
seed=a[a.index('--resume')+1]
files=list(pathlib.Path(os.environ['CLAUDE_CONFIG_DIR']).glob('projects/*/'+seed+'.jsonl'))
assert len(files)==1
prefix=[json.loads(x) for x in files[0].read_text().splitlines()]
assert all(r['sessionId']==seed for r in prefix)
assert any(r.get('isCompactSummary') for r in prefix)
assert not any(r['uuid']=='trigger-synthetic' for r in prefix)
assert sys.stdin.read()=='Inspect the synthetic fixture and propose a file update.'
mode=os.environ.get('FAKE_MODE','success')
if mode=='sleep':
    time.sleep(30)
if mode in ('401','429','503'):
    print(json.dumps({'type':'result','is_error':True,'result':'API Error: '+mode,'usage':{}}))
    sys.exit(1)
for name,data in [('Read',{'file_path':'synthetic-target'}),('Write',{'file_path':'synthetic-target','content':'synthetic'})]:
    event={'tool_name':name,'tool_input':data,'tool_use_id':'synthetic-'+name,'session_id':seed}
    r=subprocess.run([sys.executable,'-I',os.environ['SHADOW_PACK_DIR']+'/scripts/shadow_guard.py'],input=json.dumps(event),capture_output=True,text=True)
    assert r.returncode==0, r.stderr
    decision=json.loads(r.stdout)['hookSpecificOutput']['permissionDecision']
    if name=='Write' and decision=='allow':
        (root/'UNSAFE-WRITE').write_text('tool ran')
    print(json.dumps({'type':'assistant','message':{'id':'msg-'+name,'content':[{'type':'tool_use','id':'synthetic-'+name,'name':name,'input':data}]}}))
print(json.dumps({'type':'assistant','message':{'content':[{'type':'text','text':'Synthetic response mentions 429 without being an API error.'}]}}))
print(json.dumps({'type':'result','is_error':False,'session_id':seed,'usage':{'input_tokens':100,'cache_read_input_tokens':200,'cache_creation_input_tokens':300,'output_tokens':40}}))
'''


@pytest.fixture
def harness(tmp_path, monkeypatch):
    city = tmp_path / "city"
    city.mkdir()
    source_config = tmp_path / "source-config"
    source_config.mkdir()
    (source_config / "settings.json").write_text(json.dumps({"hooks": {"SessionStart": [{"hooks": [{"command": "never run"}]}]}, "enabledPlugins": {"synthetic": True}}))
    cfg_file = tmp_path / "config.toml"
    cfg_file.write_text(f'city_dir = "{city}"\nsource_config_dir = "{source_config}"\nwork_dir = "{tmp_path / "work"}"\nkill_switch = "{tmp_path / "off"}"\ntimeout_seconds = 3\n')
    cfg = common.load_config(cfg_file)
    source = tmp_path / "source.jsonl"
    rows = conversation()
    source.write_text("".join(json.dumps(r)+"\n" for r in rows))
    exe = tmp_path / "bin"
    exe.mkdir()
    fake = exe / "claude"
    fake.write_text("#!" + sys.executable + " -I\n" + FAKE_CLAUDE)
    fake.chmod(0o755)
    monkeypatch.setenv("PATH", str(exe) + os.pathsep + os.environ["PATH"])
    monkeypatch.setattr(watch, "sandbox_argv", lambda cfg, argv: argv)
    monkeypatch.setattr(watch, "normal_auth", lambda env, source: None)
    return cfg, source, rows


def run(harness):
    cfg, source, rows = harness
    return watch.run_turn(cfg, source, "synthetic", rows, (5, 9), 1, model="claude-synthetic")


def test_replay_real_subprocess_and_hook_end_to_end(harness):
    cfg, source, rows = harness
    original = source.read_bytes()
    dest, entry = run(harness)
    assert entry["status"] == "completed", entry["shadow"]["errors"]
    assert source.read_bytes() == original
    assert dest.stat().st_mode & 0o777 == 0o600
    shadow = entry["shadow"]
    assert [x["decision"] for x in shadow["decisions"]] == ["allow", "deny"]
    assert not (Path(cfg["work_dir"]) / "UNSAFE-WRITE").exists()
    assert shadow["usage"] == {"input": 100, "cache_read": 200, "cache_write": 300, "cache_write_1h": 0, "output": 40}
    assert shadow["source_integrity"]["prefix_unchanged"]
    assert Path(shadow["fork_file"]).exists()
    assert entry["diff"]["same_action"]
    assert shadow["cost_usd"] is None  # no fabricated Claude prices


@pytest.mark.parametrize("error,attempts", [("401", 1), ("429", 3), ("503", 3)])
def test_provider_failures_are_bounded_and_persisted(harness, monkeypatch, error, attempts):
    monkeypatch.setenv("FAKE_MODE", error)
    started = time.monotonic()
    _, entry = run(harness)
    assert time.monotonic() - started < 10
    assert entry["status"] == "failed"
    assert entry["shadow"]["attempts"] == attempts
    assert entry["shadow"]["errors"] == ["http_5xx" if error == "503" else "http_"+error] * attempts
    assert entry["shadow"]["source_integrity"]["prefix_unchanged"]


def test_timeout_reserves_unknown_usage_and_does_not_retry(harness, monkeypatch):
    monkeypatch.setenv("FAKE_MODE", "sleep")
    harness[0]["timeout_seconds"] = 1
    _, entry = run(harness)
    assert entry["status"] == "failed"
    assert entry["shadow"]["attempts"] == 1
    assert entry["shadow"]["unconfirmed_token_reservation"] > 0
    rollup = common.totals([(Path("synthetic"), entry)])
    assert rollup["tokens"] == 0
    assert rollup["reserved_tokens"] == entry["shadow"]["token_reservation"]
    assert rollup["budget_used_tokens"] == rollup["reserved_tokens"]


@pytest.mark.parametrize("reason", ["kill_switch", "context_window", "daily_token_cap", "non_text_trigger"])
def test_skip_never_starts_a_model(harness, reason):
    cfg, source, rows = harness
    if reason == "kill_switch":
        Path(cfg["kill_switch"]).write_text("off")
    elif reason == "context_window":
        cfg["context_window"] = 1
    elif reason == "daily_token_cap":
        cfg["daily_token_cap"] = 1
    else:
        rows[5]["message"]["content"] = [{"type": "image", "source": {"type": "base64", "data": "synthetic"}}]
    _, entry = run(harness)
    assert entry["status"] == "skipped"
    assert entry["skip_reason"] == reason
    assert "fork_file" not in entry["shadow"]


def test_summary_and_meta_are_context_not_triggers():
    rows = conversation()
    assert [rows[i]["uuid"] for i, _ in watch.completed_turns(rows, 240)] == ["role", "trigger-synthetic"]
    prefix = watch.fork_rows(rows, 5, "new-synthetic")
    assert [r["uuid"] for r in prefix] == ["role", "compact", "summary", "hook", "meta"]
    assert all(r["sessionId"] == "new-synthetic" for r in prefix)
    assert rows[0]["sessionId"] == "source-synthetic"


def test_last_turn_settles_only_after_last_assistant():
    rows = conversation()[:9]
    now = common.timestamp(rows[-1]["timestamp"])
    assert len(watch.completed_turns(rows, 240, now + 239)) == 1
    assert len(watch.completed_turns(rows, 240, now + 240)) == 2


def test_partial_json_tail_is_deferred_but_corruption_is_not(tmp_path):
    p = tmp_path / "source"
    p.write_text('{"type":"user"}\n{"type":')
    assert common.jsonl(p) == [{"type": "user"}]
    p.write_text('{"type":"user"}\nBAD\n')
    with pytest.raises(ValueError):
        common.jsonl(p)


def test_restart_does_not_backfill_unrequested_old_history(harness, monkeypatch):
    cfg, source, rows = harness
    args = type("Args", (), {"transcript": str(source), "backfill": 0, "once": True, "model": "claude-synthetic"})()
    common.private_dir(Path(cfg["work_dir"]))
    assert watch.tick(cfg, args) == 0
    assert watch.tick(cfg, args) == 0
    assert len(common.ledgers(Path(cfg["work_dir"]))) == 1


def test_one_watcher_lock(tmp_path):
    with common.lock(tmp_path):
        with pytest.raises(RuntimeError):
            common.lock(tmp_path)


def test_cst_uses_timezone_through_us_dst_change():
    assert common.cst_day("2026-10-31T18:00:00-07:00") == "2026-11-01"
    assert common.cst_day("2026-11-01T18:00:00-08:00") == "2026-11-02"


def test_accounting_does_not_double_count_stream_fragments():
    rows = [{"type": "assistant", "message": {"id": "same", "usage": {"input_tokens": 100, "output_tokens": n}}} for n in (1, 20)]
    assert watch.parse_stream(rows)[1]["input"] == 100
    assert watch.parse_stream(rows)[1]["output"] == 20
    rows.append({"type": "result", "usage": {"input_tokens": 200, "cache_creation_input_tokens": 50, "cache_creation": {"ephemeral_1h_input_tokens": 50}, "output_tokens": 40}})
    _, tokens, _ = watch.parse_stream(rows)
    assert tokens["input"] == 200 and tokens["cache_write_1h"] == 50 and tokens["cache_write"] == 0


def test_config_prices_are_used_without_inventing_unknown_prices():
    cfg = common.load_config(PACK / "assets/shadow-eval.toml")
    u = {"input": 5, "output": 10, "cache_read": 20, "cache_write": 30, "cache_write_1h": 40}
    rates = cfg["prices"][cfg["model"]]
    assert common.cost(cfg, cfg["model"], u) == sum(rates[k]*v for k,v in u.items())/1000000
    assert common.cost(cfg, "unpriced-synthetic", u) is None


def test_runtime_does_not_inherit_active_hooks_or_credentials(harness):
    root, _ = common.prepare_runtime(harness[0])
    settings = json.loads((root / "settings.json").read_text())
    assert "hooks" not in settings
    assert settings["enabledPlugins"] == {}
    assert not (root / ".credentials.json").exists()


def test_worker_routing_and_git_helper_environment_are_not_inherited(monkeypatch):
    for key in ("GC_RIG", "GC_TEMPLATE", "GC_SESSION_ID", "BASH_ENV"):
        monkeypatch.setenv(key, "synthetic-worker")
    env = common.normal_env()
    assert not any(k in env for k in ("GC_RIG", "GC_TEMPLATE", "GC_SESSION_ID", "BASH_ENV"))
    assert env["GIT_CONFIG_KEY_0"] == "core.fsmonitor"
    assert env["GIT_CONFIG_VALUE_0"] == "false"


def test_fork_provenance_checks_wire_schema_and_context(tmp_path):
    schema = {"type": "object", "properties": {"file_path": {"type": "string"}}}
    tool = {"name": "Read", "description": "synthetic read", "schema": {"input_schema": schema}}
    attachment = {"type": "prompt_snapshot", "systemPrompt": ["Synthetic system"], "tools": [tool]}
    source = [row("user", "role", "You are the mayor of this Gas City workspace. Synthetic."),
              row("attachment", "snapshot", attachment=attachment)]
    seed, runtime = tmp_path / "new-id.jsonl", tmp_path / "runtime.jsonl"
    fork = watch.fork_rows(source, len(source), seed.stem)
    common.write_private(seed, "".join(json.dumps(r)+"\n" for r in fork))
    wire = {"type": "api-request-shape", "shape": {"tools": [{"name": "Read", "description": "synthetic read", "input_schema": schema}]}}
    common.write_private(runtime, "".join(json.dumps(r)+"\n" for r in fork + [wire]))
    proof = watch.fork_evidence(source, seed, runtime)
    assert proof["prefix_rows_identical_except_session_id"]
    assert proof["system_prompt_snapshot_retained"]
    assert proof["mayor_role_in_prefix"]
    assert proof["all_source_tool_schemas_retained"]
    wire["shape"]["tools"][0]["input_schema"] = {}
    common.write_private(runtime, "".join(json.dumps(r)+"\n" for r in fork + [wire]))
    assert not watch.fork_evidence(source, seed, runtime)["all_source_tool_schemas_retained"]


def test_judge_validates_all_fields_and_rejects_freeform_identity():
    good = {"scores": dict.fromkeys(judge.CRITERIA, 2), "human_intervention_needed": False,
            "shadow_better": False, "reason": "none", "violations": []}
    assert judge.validate(good) == good
    for change in ({"reason": "Customer Synthetic Name"}, {"scores": {}}, {"shadow_better": "true"}):
        with pytest.raises(ValueError):
            judge.validate({**good, **change})


def test_scorecard_three_synthetic_turns_contains_required_fields(tmp_path):
    items, judgments = [], []
    for i in range(3):
        entry = {"trigger_uuid": f"synthetic-{i}", "status": "completed", "diff": {"same_action": i != 0},
                 "shadow": {"usage": {"input": 10, "output": 20}, "wall_seconds": i + 1, "cost_usd": None}}
        items.append((tmp_path / str(i), entry))
        judgments.append({"turn_id": entry["trigger_uuid"], "usage": {"input": 100}, "cost_usd": None,
                          "verdict": {"scores": dict.fromkeys(judge.CRITERIA, i), "human_intervention_needed": i == 0,
                                      "shadow_better": i == 2, "reason": "rule_violation" if i == 0 else "better_grounding",
                                      "violations": ["rule_violation"] if i == 0 else []}})
    card = judge.scorecard("2026-10-28", "opus", items, judgments, [])
    assert card["criterion_means"] == dict.fromkeys(judge.CRITERIA, 1)
    assert card["same_decision_percent"] == pytest.approx(200/3)
    assert card["violation_count"] == 1 and card["human_intervention_count"] == 1
    assert card["tokens"] == 90 and card["latency_median_seconds"] == 2
    assert card["turns_skipped"] == card["turns_failed"] == 0
    assert len(card["worst_examples"]) == len(card["shadow_better_examples"]) == 1
    md = judge.markdown(card)
    assert "Claude-family" in md and "--engine codex --sample 10" in md
    assert "synthetic-0" in md


def test_codex_spot_check_reuses_exact_saved_opus_packet(harness, monkeypatch, tmp_path):
    cfg, _, _ = harness
    root = Path(cfg["work_dir"])
    prior = root / "judgments/synthetic-1/prior"
    prior.mkdir(parents=True)
    old_packet = {"trigger": "Synthetic trigger", "do_nots": "OLD synthetic rule", "memory_index": "OLD synthetic memory", "fable": [], "shadow": []}
    (prior / "packet.json").write_text(json.dumps(old_packet))
    (prior / "judgment.json").write_text(json.dumps({"engine": "opus"}))
    fake = tmp_path / "codex-shim"
    verdict = {"scores": dict.fromkeys(judge.CRITERIA, 2), "human_intervention_needed": False, "shadow_better": False, "reason": "none", "violations": []}
    fake.write_text("#!" + sys.executable + " -I\n" +
        "import json,sys,pathlib,os\n" +
        "a=sys.argv[1:]\nassert a[:2]==['-p','city']\nassert a[a.index('--sandbox')+1]=='read-only'\n" +
        "assert '--output-schema' in a and '--ephemeral' in a\n" +
        "p=sys.stdin.read()\nassert 'OLD synthetic rule' in p\n" +
        "assert 'CODEX_HOME' in os.environ\n" +
        "pathlib.Path(a[a.index('--output-last-message')+1]).write_text(" + repr(json.dumps(verdict)) + ")\n")
    fake.chmod(0o755)
    cfg["codex_shim"] = str(fake)
    monkeypatch.setattr(judge, "sandbox_argv", lambda cfg, argv: argv)
    monkeypatch.setattr(judge, "prepare_codex_runtime", lambda cfg: tmp_path / "private-codex")
    monkeypatch.setattr(judge, "packet", lambda *args: pytest.fail("must reuse the saved packet"))
    result = judge.judge_one(cfg, {"trigger_uuid": "synthetic-1"}, "codex")
    assert json.loads(Path(result["packet_file"]).read_text()) == old_packet
    assert result["verdict"] == verdict
    assert result["usage_known"] is False


@pytest.mark.parametrize("name", ["watch", "judge", "status"])
def test_real_command_entrypoints_use_isolated_python(name):
    script = PACK / "commands" / ("shadow-" + name + ".sh")
    assert "python3 -I" in script.read_text()
    r = subprocess.run([str(script), "--help"], capture_output=True, text=True)
    assert r.returncode == 0 and "usage:" in r.stdout

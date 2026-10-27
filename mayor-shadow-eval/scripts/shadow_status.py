#!/usr/bin/env -S python3 -I
"""Print content-free source health, coverage, storage and today's accounting."""
import argparse
import json
import datetime as dt
from pathlib import Path
import sys
sys.dont_write_bytecode = True
sys.path.insert(0, str(Path(__file__).resolve().parent))
from shadow_common import cst_day, ledgers, load_config, path, totals, switch_on, source_info, discover


def status(cfg, day):
    root = path(cfg['work_dir'])
    result = totals(ledgers(root, day))
    health = root / 'watch-health.json'
    observed = json.loads(health.read_text()) if health.exists() else {}
    try:
        source, by = (path(observed["source"]), observed.get("discovery")) if observed.get("source") else discover(cfg)
        observed.update(source_info(source), discovery=by)
    except (OSError, ValueError, RuntimeError):
        observed['last_error'] = 'source_unavailable'
    result.pop('latencies')
    sampling = observed.get('sampling', {})
    result.update({k: observed.get(k) for k in ('source_basename', 'discovery', 'newest_trigger_age_seconds', 'stale')})
    result.update(date_cst=day, switch='on' if switch_on(cfg['kill_switch']) else 'off',
                  health='RED' if observed.get('stale') or observed.get('last_error') else 'OK',
                  last_error_class='stale_source' if observed.get('stale') else observed.get('last_error') or result['last_error'],
                  target_turns_per_day=cfg['target_turns_per_day'],
                  expected_coverage=sampling.get('expected_coverage'),
                  estimated_eligible_per_day=sampling.get('estimated_eligible_per_day'),
                  runtime_bytes=sum(p.stat().st_size for p in (root / 'runtime').rglob('*') if p.is_file() and not p.is_symlink()))
    return result


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('--config', default='~/city/ops/shadow-eval.toml')
    p.add_argument('--date', default=cst_day())
    a = p.parse_args()
    dt.date.fromisoformat(a.date)
    print(json.dumps(status(load_config(a.config), a.date), indent=2))


if __name__ == '__main__':
    main()

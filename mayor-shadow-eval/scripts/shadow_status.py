#!/usr/bin/env -S python3 -I
"""Print today's replay totals without transcript text or raw errors."""
import argparse
import json
import datetime as dt
from pathlib import Path
import sys
sys.dont_write_bytecode = True
sys.path.insert(0, str(Path(__file__).resolve().parent))
from shadow_common import cst_day, ledgers, load_config, path, totals


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("--config", default="~/city/ops/shadow-eval.toml")
    p.add_argument("--date", default=cst_day())
    a = p.parse_args()
    dt.date.fromisoformat(a.date)
    cfg = load_config(a.config)
    result = totals(ledgers(path(cfg["work_dir"]), a.date))
    health = path(cfg["work_dir"]) / "watch-health.json"
    if health.exists():
        result["watcher_error"] = json.loads(health.read_text()).get("last_error")
    result.pop("latencies")
    result.update({"date_cst": a.date, "kill_switch": path(cfg["kill_switch"]).exists()})
    print(json.dumps(result, indent=2))


if __name__ == "__main__":
    main()

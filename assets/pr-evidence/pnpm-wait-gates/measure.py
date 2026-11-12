"""Run the actual three-caller test with CPU load and per-caller timing.

Run from gascity/: python3 ../assets/pr-evidence/pnpm-wait-gates/measure.py
Only disposable fixture copies are instrumented. Repository shims are untouched.
"""

import argparse
import concurrent.futures
import importlib.util
import json
import os
from pathlib import Path
import shlex
import subprocess
import sys
import threading
import time


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--runs", type=int, required=True)
    parser.add_argument("--jobs", type=int, default=8)
    parser.add_argument("--burners", type=int, default=32)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    source = Path("tests/test_toolchain_shims.py").resolve()
    spec = importlib.util.spec_from_file_location("shim_tests", source)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    original_init = module.Fixture.__init__
    original_env = module.Fixture.env
    original_run = module.Fixture.run
    original_start = getattr(module.Fixture, "start", None)
    fixtures = threading.local()

    def init(fx, *pos, **kw):
        original_init(fx, *pos, **kw)
        fixtures.current = fx
        fx.measure_origin = time.monotonic()
        fx.measure_results = {}
        fx.measure_events = fx.root / "events.jsonl"
        stamp = fx.root / "stamp.py"
        stamp.write_text(
            "import json, os, sys, time\n"
            "with open(os.environ['MEASURE_EVENTS'], 'a') as f:\n"
            " f.write(json.dumps([time.monotonic(), os.environ.get('MEASURE_CALLER', 'unknown'), sys.argv[1]]) + '\\n')\n"
        )
        trace = f"{shlex.quote(sys.executable)} {shlex.quote(str(stamp))}"
        shim = fx.shims / "pnpm"
        code = shim.read_text()
        code = code.replace("#!/bin/sh\n", f"#!/bin/sh\n{trace} process_start\n", 1)
        code = code.replace("take_lane_lock() {\n", f"take_lane_lock() {{\n    {trace} lock_attempt\n", 1)
        code = code.replace('        [ "$rc" -ne 0 ] || return 0', f'        [ "$rc" -ne 0 ] || {{ {trace} gate_acquired; return 0; }}', 1)
        code = code.replace('        gate_waited=$((gate_waited + 1))', f'        {trace} gate_waited\n        gate_waited=$((gate_waited + 1))', 1)
        code = code.replace('    claim_dir "$LOCK" LOCKED || rc=$?', f'    claim_dir "$LOCK" LOCKED || rc=$?\n    {trace} lane_claim_$rc', 1)
        shim.write_text(code)
        code = fx.fake_pnpm.read_text()
        code = code.replace("    install|i|ci|add|update|it|install-test)\n", f"    install|i|ci|add|update|it|install-test)\n        {trace} install_start\n", 1)
        code = code.replace('        echo "fake pnpm: installed"', f'        {trace} install_end\n        echo "fake pnpm: installed"', 1)
        code = code.replace('echo "fake pnpm ran: $*"', f'{trace} command_start\necho "fake pnpm ran: $*"', 1)
        fx.fake_pnpm.write_text(code)

    def env(fx, **extra):
        extra["MEASURE_EVENTS"] = str(fx.measure_events)
        extra["MEASURE_CALLER"] = getattr(fixtures, "caller", "unknown")
        return original_env(fx, **extra)

    def run(fx, prog, *argv, **extra):
        name = argv[-1]
        fixtures.caller = name
        requested = time.monotonic()
        launched = time.monotonic()
        result = original_run(fx, prog, *argv, **extra)
        fx.measure_results[name] = {
            "requested": requested - fx.measure_origin,
            "launched": launched - fx.measure_origin,
            "completed": time.monotonic() - fx.measure_origin,
            "returncode": result.returncode,
            "waited": "waiting for another caller's lane install" in result.stderr,
        }
        return result

    module.Fixture.__init__ = init
    module.Fixture.env = env
    module.Fixture.run = run
    if original_start is not None:
        def start(fx, prog, *argv, **extra):
            name = argv[-1]
            fixtures.caller = name
            requested = time.monotonic()
            launched = time.monotonic()
            call = original_start(fx, prog, *argv, **extra)
            original_finish = call.finish

            def finish():
                result = original_finish()
                fx.measure_results[name] = {
                    "requested": requested - fx.measure_origin,
                    "launched": launched - fx.measure_origin,
                    "completed": time.monotonic() - fx.measure_origin,
                    "returncode": result.returncode,
                    "waited": "waiting for another caller's lane install" in result.stderr,
                }
                return result

            call.finish = finish
            return call

        module.Fixture.start = start

    def trial(index):
        case = module.PnpmShimTests("test_concurrent_callers_wait_for_one_install_instead_of_racing_it")
        # Capture before TemporaryDirectory cleanup; run the unchanged test body.
        case.setUp()
        fx = fixtures.current
        result = {"run": index, "passed": True}
        try:
            case.test_concurrent_callers_wait_for_one_install_instead_of_racing_it()
        except Exception as error:
            result.update(passed=False, error_type=type(error).__name__)
        finally:
            result["callers"] = fx.measure_results
            result["events"] = [
                {"time": round(t - fx.measure_origin, 6), "caller": c, "event": e}
                for t, c, e in (json.loads(line) for line in fx.measure_events.read_text().splitlines())
            ]
            result["installs"] = len(fx.installs())
            result["commands"] = fx.argv()
            case.tearDown()
        return result

    burners = []
    started = time.monotonic()
    try:
        for _ in range(args.burners):
            burners.append(subprocess.Popen([sys.executable, "-c", "while True: pass"], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL))
        with args.output.open("w") as output, concurrent.futures.ThreadPoolExecutor(max_workers=args.jobs) as pool:
            failures = 0
            for result in pool.map(trial, range(1, args.runs + 1)):
                failures += not result["passed"]
                output.write(json.dumps(result, sort_keys=True) + "\n")
                output.flush()
                print(f"run={result['run']} passed={result['passed']} failures={failures}", flush=True)
    finally:
        for burner in burners:
            burner.terminate()
        for burner in burners:
            burner.wait()
    print(json.dumps({"runs": args.runs, "failures": failures, "seconds": round(time.monotonic() - started, 3), "jobs": args.jobs, "burners": args.burners, "logical_cpus": os.cpu_count()}))
    return bool(failures)


if __name__ == "__main__":
    sys.exit(main())

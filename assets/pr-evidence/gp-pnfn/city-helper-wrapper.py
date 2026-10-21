#!/usr/bin/env python3
# gp-pnfn: pinned installation of the pack helper and its shared redactor.
# A missing module/rule file fails closed, before any Slack text is read.
from pathlib import Path
import os
import runpy
import sys

scripts = Path(__file__).resolve().parent / "slack-tail-redaction" / "6849dd900e221f01" / "slack-full/scripts"
os.environ.setdefault("GC_CITY_PATH", str(Path(__file__).resolve().parents[1]))
sys.path.insert(0, str(scripts))
runpy.run_path(str(scripts / "slack_tail.py"), run_name="__main__")

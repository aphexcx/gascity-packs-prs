#!/usr/bin/env -S python3 -I
"""Replay the captured prompt attachments; never invoke a live gc hook."""
import os
from pathlib import Path
import sys

try:
    target = Path(os.environ['SHADOW_REPLAY_CONTEXT']).resolve()
    root = Path(os.environ['SHADOW_WORK_DIR']).resolve()
    if not target.is_relative_to(root):
        raise ValueError('outside private runtime')
    sys.stdout.write(target.read_text())
except Exception:
    print('SHADOW EVAL: captured prompt context unavailable.', file=sys.stderr)
    raise SystemExit(2)

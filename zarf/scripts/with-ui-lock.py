#!/usr/bin/env python3
"""Keep UI publication and Go's embedded-file reads mutually exclusive."""

import fcntl
import os
from pathlib import Path
import sys


root = Path(__file__).resolve().parents[2]
lock_path = root / "tmp" / "ui-build.lock"
lock_path.parent.mkdir(parents=True, exist_ok=True)
with lock_path.open("a") as lock:
    fcntl.flock(lock, fcntl.LOCK_EX)
    # The command owns the lock, including when its launcher is interrupted.
    os.set_inheritable(lock.fileno(), True)
    os.execvp(sys.argv[1], sys.argv[1:])

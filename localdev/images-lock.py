"""images-lock.py COMMAND [ARG...]

Runs COMMAND while holding this machine's images lock, waiting for whoever holds it.

A build tags an image and pushes it a moment later, and the prune that ends another build
removes every tag that is not in use, which a tag between its build and its push is not yet. So
each target that builds, and the prune, runs under this lock, one at a time. A command started
under the lock already holds it and runs as it is. The lock goes with the process that took it,
so a build that dies leaves nothing to clear.
"""
import fcntl
import os
import subprocess
import sys

HELD = "HL_IMAGES_LOCK_HELD"
PATH = "/tmp/hl-images.lock"

command = sys.argv[1:]
if not command:
    sys.exit(__doc__)
if os.environ.get(HELD):
    os.execvp(command[0], command)
lock = open(PATH, "w")
try:
    fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
except BlockingIOError:
    print(f"images-lock: another build or prune holds {PATH}, waiting for it", file=sys.stderr)
    fcntl.flock(lock, fcntl.LOCK_EX)
sys.exit(subprocess.run(command, env={**os.environ, HELD: "1"}).returncode)

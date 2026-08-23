#!/usr/bin/env python3
"""
Termination-policy e2e workload: a job that checkpoints its step count when it is told
termination is coming, and resumes from that step when it starts again.

The pair of behaviours only means anything together, so both live here:

  on start-up  it reads step.txt from its OWN prefix ($HYPOTHESISLOOP_DATA_URI) and continues
               from that step, or finds nothing there and begins at zero. It reports the step it
               began at as `resume_step` — 0 on a first run, the checkpointed step on a resumed
               one. That number is the whole proof: a job that came back from zero reports 0
               twice, and no amount of "it completed" says otherwise.
  on SIGTERM   it waits CHECKPOINT_WRITE_DELAY_SECONDS and only THEN writes step.txt.
               Deliberately: the ordinary shutdown grace is 5s (see
               default_termination_grace_period_seconds), so a delay above it means the
               checkpoint can only land if the job was actually GRANTED its declared
               checkpoint window. A build that signalled the job but killed it on the ordinary
               grace produces no checkpoint, no resume, and a failing scenario — which is
               exactly the distinction the window exists to make.

The step series itself is reported as `train_step`, one point per step, so a scenario can read
whether the series CONTINUED across the two stints (STEPS_TOTAL points in all) or RESTARTED
(more than that, with the early steps reported twice).

S3 is spoken through data_stage.py's signer rather than a second one. The signing is not
incidental — the session token is part of the signature, so a checkpoint this file writes is
written under the scoped session the platform handed the job, into the prefix keyed on the
job's experiment id. That the id survives a requeue is what makes resumption need no platform
state at all.

Environment (injected by the runtime, see runtime/k8s/internal/k8sexec/job_build.go):
  HYPOTHESISLOOP_DATA_URI     s3://bucket/<pe>/<agent>/<experiment>/ — same address on both stints
  AWS_*                       the scoped session data_stage.sign() signs every request with
  HYPOTHESISLOOP_ATTEMPT      which attempt this is; reported, never used to decide anything
"""

import os
import signal
import sys
import time

from data_stage import DATA_URI, log, object_path, post_metric, s3, split_s3_uri

STEP_KEY = "step.txt"
# How many steps a full run is. The scenario preempts partway through and asserts on how many
# points came back, so this is a count the scenario shares rather than a duration.
STEPS_TOTAL = int(os.environ.get("STEPS_TOTAL", "14"))
STEP_SECONDS = int(os.environ.get("STEP_SECONDS", "5"))
# Above the ordinary shutdown grace, so that a checkpoint landing at all proves a window was
# granted rather than merely that SIGTERM was delivered. See the module docstring.
CHECKPOINT_WRITE_DELAY_SECONDS = int(os.environ.get("CHECKPOINT_WRITE_DELAY_SECONDS", "8"))

ATTEMPT = int(os.environ.get("HYPOTHESISLOOP_ATTEMPT", "0"))

BUCKET, PREFIX = split_s3_uri(DATA_URI)
STEP_PATH = object_path(BUCKET, f"{PREFIX}{STEP_KEY}")

# The step the loop below is currently on, read by the signal handler. A module-level int rather
# than anything cleverer because the handler runs on the main thread between bytecodes and needs
# the value as of the instant it fired.
current_step = 0


def read_checkpoint() -> int:
    """The step this job stopped at last time, or 0 if it never ran before.

    A missing object is the ordinary case for a first run, so it is an answer rather than an
    error. Any other non-200 IS an error: silently starting from zero because the store was
    unreachable would turn a broken checkpoint into a job that merely looks slow.
    """
    status, body = s3("GET", STEP_PATH)
    if status == 404:
        log("no checkpoint under this job's prefix — starting from step 0")
        return 0
    if status != 200:
        raise SystemExit(f"reading own checkpoint failed with status {status}: {body[:400]!r}")
    step = int(body.decode().strip())
    log(f"resuming from checkpointed step {step}")
    return step


def write_checkpoint(step: int) -> int:
    status, body = s3("PUT", STEP_PATH, body=str(step).encode())
    log(f"wrote checkpoint step={step} -> {status}")
    if status != 200:
        raise SystemExit(f"writing checkpoint failed with status {status}: {body[:400]!r}")
    return status


def on_termination(signum, frame) -> None:
    """The checkpoint window, used. Nothing here was told what to do — the platform reported that
    termination is coming, and this is what this job chose to do with the interval."""
    log(f"signal {signum}: termination is coming, checkpointing step {current_step}")
    # Deliberately slower than the ordinary shutdown grace: see the module docstring.
    time.sleep(CHECKPOINT_WRITE_DELAY_SECONDS)
    status = write_checkpoint(current_step)
    post_metric("checkpoint_write_status", float(status))
    post_metric("checkpoint_step", float(current_step))
    # 0, not 143: this job was not failed, it was told to stop and it stopped cleanly. The
    # experiment's own status is the control plane's to decide and is already decided.
    sys.exit(0)


def main() -> None:
    global current_step
    signal.signal(signal.SIGTERM, on_termination)

    start = read_checkpoint()
    current_step = start
    log(f"starting at step {start}/{STEPS_TOTAL} (attempt {ATTEMPT})")
    # Reported before any work, so it is on the record even for a stint that is terminated
    # immediately. 0 on a first run and the checkpointed step on a resumed one — the one number
    # that separates "resumed" from "started over and finished anyway".
    post_metric("resume_step", float(start), start / STEPS_TOTAL if STEPS_TOTAL else 0.0)

    for step in range(start, STEPS_TOTAL):
        time.sleep(STEP_SECONDS)
        current_step = step + 1
        fraction = current_step / STEPS_TOTAL
        # One point per step, once. A run that restarted from zero reports the early steps a
        # second time, which is what makes the point COUNT (not the maximum) the discriminating
        # reading of "the series continued".
        post_metric("train_step", float(current_step), fraction)
        post_metric("val_accuracy", 0.5 + 0.4 * fraction, fraction)

    log(f"done — reached step {current_step}/{STEPS_TOTAL}")


if __name__ == "__main__":
    main()

"""Platform updates about the agent's own resources, delivered by the harness.

The harness runs `hl watch` whenever the agent ends its turn, and briefly after each of its tool
calls: the platform holds every change for a week, and hl reads on from the id of the last change
it saved as its cursor, so a batch waits server-side instead of in this process, survives a
restart, and is delivered once whichever of the two hands it over, which is when its cursor moves.
A cursor older than what the platform holds is handed over as one watch.reset line, which always
wakes: what happened in between is read from the situation. An event is a
pointer; a job that ended is handed over with the machine it ran on and its metrics, so that
learning of it costs no call. What stands around the batch (time, capacity, budget, standings,
jobs and subagents in flight) comes as the situation with every prompt that starts a turn.
"""
from __future__ import annotations

import asyncio
import json
import logging
import os
import shutil
import time
from collections.abc import Callable

from hypothesisloop_agent import config, outage, platform

log = logging.getLogger("llm_agent")

HL_WATCH = ["hl", "watch"]
_RETRY_S = 30.0
# A drain gives up after this long on a platform that does not answer.
_DRAIN_S = 5

# The kinds that wake whoever ended its turn, per role, and the one place that says so. An event
# of another kind stays on the platform (hl watch --wake) and is handed over with the next that
# wakes, after the next tool call, or with a coordinator's review. A coordinator has to learn
# within its COALESCE_S that an agent finished or has gone idle (agent.idle=new: no job of its own in
# flight for the platform's notice, sent to that agent too and again, as agent.idle=continues,
# every notice period the stretch lasts; a repeat rides along with the coordinator's next batch
# and its minutes show in the situation), that the capacity holds places beyond the run's queued
# jobs, whatever the best stands at against the target (capacity.idle=new, again as
# capacity.idle=continues every notice period it lasts), that a job is held by what waiting will
# not clear or has waited past its own estimate (job.held, sent once, to the job's agent too),
# that a stage advanced, that someone wrote a comment, that the description or the baseline changed (a raised
# baseline is the bar every researcher is set against from then on), that the run closed or that its
# status_reason began to wait for the coordinator (platform_experiment.status, sent again with
# the reason as its detail, as when the last researcher's machine stops for good). An agent has
# to learn the same of its own jobs ending, and at once of a message the coordinator or the owner
# sent it (agent.message), which wakes no coordinator: its stream echoes the ones it sent. The owner changing the goal
# (platform_experiment.goal) wakes whoever acts on it: a draft's planner, which plans again, or the
# coordinator, which brings the description in line; one addressed to a planner is its own and reaches no
# researcher. A coordinator is woken too by the owner changing fields of the plan
# (platform_experiment.plan). A coordinator learns at once of an idea a person
# posted (hypothesis.new of a user, a key or staff): every one is to be assigned or answered. A
# message of the owner (agent.message=owner) wakes it whether sent to it or to a researcher: either
# is direction the steering has to take in; its own messages come back as coordinator.
# A job submitted on a hypothesis another agent's job tests (job.duplicate) wakes it too: two
# seats on one idea are one seat wasted until it gives one of them another direction. So does a
# researcher the platform added in the place of one that ended (agent.replaced): it starts with
# no direction but its predecessor's hyperparameters.
# Both learn at once that their own agent type was edited and they were moved to its new version
# (agent.instructions): the harness applies it at the turn boundary and tells the agent nothing
# of it. A coordinator is not woken by its researchers' edits (KIND@SUBJECT matches the subject,
# which is COORDINATOR for its own): it reads them in the situation and with its next batch.
# Everything else is read when next acting. A kind wakes only for what its reader can act on.
WAKES = {
    config.COORDINATOR: (
        platform.EVENT_AGENT_FINISHED, f"{platform.EVENT_AGENT_IDLE}={platform.IDLE_NEW}", f"{platform.EVENT_CAPACITY_IDLE}={platform.IDLE_NEW}",
        platform.EVENT_EXPERIMENT_BLOCKED, platform.EVENT_JOB_HELD, platform.EVENT_STAGE_BOUNDARY, platform.EVENT_COMMENT_NEW,
        platform.EVENT_PLATFORM_EXPERIMENT_STATUS, platform.EVENT_PLATFORM_EXPERIMENT_DESCRIPTION,
        platform.EVENT_PLATFORM_EXPERIMENT_BASELINE, platform.EVENT_PLATFORM_EXPERIMENT_GOAL, platform.EVENT_PLATFORM_EXPERIMENT_PLAN,
        f"{platform.EVENT_HYPOTHESIS_NEW}={platform.ACTOR_USER}",
        f"{platform.EVENT_HYPOTHESIS_NEW}={platform.ACTOR_KEY}", f"{platform.EVENT_HYPOTHESIS_NEW}={platform.ACTOR_STAFF}",
        f"{platform.EVENT_AGENT_MESSAGE}={platform.MESSAGE_FROM_OWNER}", platform.EVENT_JOB_DUPLICATE, platform.EVENT_AGENT_REPLACED,
        f"{platform.EVENT_AGENT_INSTRUCTIONS}@{config.COORDINATOR}"),
    "agent": (
        platform.EVENT_EXPERIMENT_STATUS, platform.EVENT_EXPERIMENT_BLOCKED, platform.EVENT_JOB_HELD,
        platform.EVENT_JOB_UNDERPERFORMING, platform.EVENT_AGENT_CUT, platform.EVENT_AGENT_IDLE, platform.EVENT_AGENT_MESSAGE,
        platform.EVENT_STAGE_BOUNDARY, platform.EVENT_COMMENT_NEW, platform.EVENT_PLATFORM_EXPERIMENT_STATUS,
        platform.EVENT_PLATFORM_EXPERIMENT_DESCRIPTION, platform.EVENT_PLATFORM_EXPERIMENT_BASELINE,
        platform.EVENT_PLATFORM_EXPERIMENT_GOAL, platform.EVENT_AGENT_INSTRUCTIONS),
}
# A message to one agent from the coordinator or the owner, told as what it says.
MESSAGE = platform.EVENT_AGENT_MESSAGE
# What hl prints, in place of the changes, when the platform no longer holds those after its cursor.
RESET = platform.WATCH_RESET
# The owner changed the goal. The event is a pointer whose value is the new goal_version: a planner
# is told the goal itself, read from the draft (core.revising), in place of the event.
GOAL = platform.EVENT_PLATFORM_EXPERIMENT_GOAL
# What wakes a coordinator besides, while its platform experiment is open: a job that failed then
# is a baseline agent's, and one that failed for what the description says is the coordinator's to
# repair. Three failed in a row before a coordinator heard of any, from the agent giving up. One
# that completed is acted on when its agent finishes, which wakes by itself.
OPEN_WAKES = (f"{platform.EVENT_EXPERIMENT_STATUS}={platform.STATUS_FAILED}",)

# An agent that ended its turn waits for an update with no limit: the platform's agent.idle, which
# lives in its store and outlasts a restart, is the one timer for an agent with nothing in flight.
# A coordinator reviews the run every REVIEW_S that nothing woke it, without doubling: a lead
# rested for hours while a doubling nudge waited.
REVIEW_S = 1200
REVIEW_PROMPT = ("Nothing woke you for {minutes} minutes: this is your regular review. Judge from the situation "
                 "whether the run reaches its target in the time left, and act where it does not.")
SUMMARY_STALE = "{jobs} jobs completed since you last wrote the summary: add what they found."
IDEAS_OPEN = ("{ideas} ideas from people are still open (`hl hypothesis list --by people`): assign each to a researcher "
              "as places free, or answer it with a comment that gives the evidence why not.")
# A coordinator's batch is held this long after the event that woke it, so that what follows it
# closely comes in the same wake. An agent's comes at once: it has no window for messages.
COALESCE_S = 90
# Said to an agent at every end of a turn with no job of its own submitted, queued or running and
# no subagent at work: it would otherwise wait for an update that cannot come.
NOTHING_IN_FLIGHT = ("The platform holds no job of yours submitted, queued or running and no subagent of yours is at work, so no update of "
                     "your own is coming. Put work in flight, or end your run with "
                     "`hl platform-experiments agents finish --reason TEXT`.")
# A situation read that takes longer is given up, and the prompt says so in its place.
_SITUATION_S = 30


_AGENT_ID = "HL_WATCH_AGENT_ID"
# Where hl keeps the cursor of the agent's update feed, in the harness's state directory.
STATE_DIR = ".hl-state"


def watch_env(cfg: config.Config, statedir: str) -> dict[str, str]:
    """The environment hl watch needs: the API, the token, which says to hl who is watching, and
    a state dir that outlives the process, with the agent the token is, "" for a coordinator."""
    return {**os.environ, platform.ENV_API_URL: cfg.api_url, platform.ENV_TOKEN: cfg.token,
            "HL_STATE_DIR": os.path.join(os.path.abspath(statedir), STATE_DIR),
            _AGENT_ID: "" if config.coordinates(cfg) else cfg.agent_id}


_DELIVERED_TO = "HL_STATE_DIR_DELIVERED"
_TAKEN_AT = "HL_STATE_TAKEN_AT"


def _cursors(state: str) -> str:
    if not os.path.isdir(state):
        return "{}"
    found = {}
    for name in sorted(os.listdir(state)):
        if name.endswith(".json"):
            with open(os.path.join(state, name)) as f:
                found[name] = f.read()
    return json.dumps(found)


def taking(env: dict[str, str], reader: str) -> dict[str, str]:
    """env whose hl watch moves the cursor in reader's copy of hl's state, put in place by
    delivered() once what it read was handed over. A batch never handed over, as when the wait
    for it ended or the session died first, is read again."""
    state = env["HL_STATE_DIR"]
    copy = f"{state}.{reader}"
    shutil.rmtree(copy, ignore_errors=True)
    if os.path.isdir(state):
        shutil.copytree(state, copy)
    else:
        os.makedirs(copy)
    return {**env, "HL_STATE_DIR": copy, _DELIVERED_TO: state, _TAKEN_AT: _cursors(state)}


def superseded(env: dict[str, str]) -> bool:
    """Whether another reader delivered since env was taken: what env read was handed over
    by that reader, or is read again from where it left the cursor."""
    return _cursors(env[_DELIVERED_TO]) != env[_TAKEN_AT]


def delivered(env: dict[str, str]) -> bool:
    """Moves the cursor that the watches with env, from taking(), advanced into place, unless
    superseded: a cursor never moves back. Whether it moved."""
    if superseded(env):
        return False
    os.makedirs(env[_DELIVERED_TO], exist_ok=True)
    for name in os.listdir(env["HL_STATE_DIR"]):
        if name.endswith(".json"):
            os.replace(os.path.join(env["HL_STATE_DIR"], name), os.path.join(env[_DELIVERED_TO], name))
    return True


def _agent(env: dict[str, str]) -> str:
    return env[_AGENT_ID]


def watch_command(env: dict[str, str]) -> list[str]:
    """An agent watches its own resources. A coordinator watches the whole platform experiment,
    narrowed to what may need a decision."""
    if _agent(env):
        return HL_WATCH
    return [*HL_WATCH, "--all", "--decisions"]


async def _run_hl(command: list[str], env: dict[str, str], stopped: Callable[[], bool],
                  warn: bool = True) -> tuple[int | None, str, str]:
    """The exit code of command, None when stopped() ended it, its stdout and what it said on
    stderr."""
    proc = await asyncio.create_subprocess_exec(*command, env=env, stdout=asyncio.subprocess.PIPE,
                                                stderr=asyncio.subprocess.PIPE)
    communicate = asyncio.ensure_future(proc.communicate())
    try:
        while not communicate.done():
            if stopped():
                proc.kill()
                await communicate
                return None, "", ""
            await asyncio.wait({communicate}, timeout=1)
        stdout, stderr = communicate.result()
    finally:
        if proc.returncode is None:
            proc.kill()
            await proc.wait()
    said = stderr.decode(errors="replace").strip()
    if proc.returncode != 0 and warn:
        _warn(command, proc.returncode, said)
    return proc.returncode, stdout.decode(errors="replace"), said


def _warn(command: list[str], code: int, said: str) -> None:
    log.warning("%s exited %s: %s", " ".join(command[:3]), code, said[-500:])


def _events(stdout: str) -> list[dict]:
    return [json.loads(line) for line in stdout.splitlines() if line.startswith("{")]


_UNFINISHED = sorted(platform.EXPERIMENT_STATUSES - platform.ENDED_STATUSES)


def _result(job: dict) -> str:
    """A COMPLETED job's metrics as `hl job wait` prints them and, when it ranks nowhere, why."""
    said = json.dumps(job.get("metrics", {}), separators=(",", ":"))
    return said + (f", ranks nowhere: {job['not_rankable_reason']}" if job.get("rankable") is False else "")


async def _with_job_state(events: list[dict], env: dict[str, str], hl: str) -> list[dict]:
    """events, completed with what whoever is told of an ended job would otherwise read: the
    machine it ran on, and for a COMPLETED one its declared metrics and, when it ranks nowhere,
    why, as its event's detail, from one `hl job wait` per batch, which answers at once for jobs
    that ended. A call that failed adds the event that says what was not read and the command
    that reads it: told COMPLETED with no metric and nothing else, an agent took the job for one
    that reported none."""
    def ended(e: dict) -> bool:
        return e["kind"] == platform.EVENT_EXPERIMENT_STATUS and e["value"] in platform.ENDED_STATUSES

    ids = list(dict.fromkeys(e["subject"] for e in events if ended(e)))
    if not ids:
        return events
    command = [hl, "job", "wait", *ids]
    code, stdout, _ = await _run_hl(command, env, lambda: False)
    if code != 0:
        return events + [{"kind": "not_read", "value": "the metrics and machine of the jobs that ended: "
                                                       f"`{' '.join(['hl', *command[1:]])}` reads it"}]
    jobs = {job["id"]: job for job in _events(stdout)}

    def told(e: dict) -> dict:
        job = jobs.get(e["subject"]) if ended(e) else None
        if job is None:
            return e
        return dict(e, machine=job.get("ran_on", ""), **({"detail": _result(job)} if e["value"] == platform.STATUS_COMPLETED else {}))

    return [told(e) for e in events]


def _unfinished_read(agent: str) -> list[str]:
    return ["job", "stats", "--agent", agent]


def _unfinished(stats: dict) -> dict[str, int]:
    """The agent's job counts by unfinished status, those it has none of left out. hl prints no
    by_status for an agent with no job, as it leaves out every empty object."""
    by_status = stats.get("by_status", {})
    return {s: by_status[s] for s in _UNFINISHED if by_status.get(s)}


async def jobs_in_flight(env: dict[str, str], hl: str = "hl") -> int | None:
    """How many jobs of the agent watching with env are queued or running, None when that could
    not be read."""
    command = [hl, *_unfinished_read(_agent(env))]
    code, stdout, said = await _run_hl(command, env, lambda: False, warn=False)
    stats = _events(stdout) if code == 0 else []
    if not stats:
        if not outage.under_way():
            _warn(command, code, said or "no stats in the answer")
        return None
    return sum(_unfinished(stats[0]).values())


async def situation(env: dict[str, str], hl: str = "hl") -> str:
    """`hl situation` as it prints it now, under a heading, for the end of a prompt. A read that
    failed is said in its place: the wake goes on without it."""
    deadline = time.monotonic() + _SITUATION_S
    code, stdout, said = await _run_hl([hl, "situation"], env, lambda: time.monotonic() > deadline)
    if code == 0 and stdout.strip():
        return "Situation:\n" + stdout.strip()
    if code is None:
        said = f"it did not answer in {_SITUATION_S}s"
        log.warning("hl situation: %s", said)
    why = (said.splitlines() or [f"it exited {code}"])[-1]
    return f"Situation: not read, `hl situation` failed: {why[-300:]}"


async def standing(env: dict[str, str], hl: str = "hl") -> dict | None:
    """`hl situation --json` as the platform answers it, None when it could not be read."""
    deadline = time.monotonic() + _SITUATION_S
    code, stdout, _ = await _run_hl([hl, "situation", "--json"], env, lambda: time.monotonic() > deadline)
    if code != 0:
        return None
    try:
        return json.loads(stdout)
    except ValueError:
        log.warning("hl situation --json: no JSON in the answer")
        return None


def review_prompt(minutes: int, situation: dict | None) -> str:
    """The regular review, with the findings the summary does not hold yet and the open ideas of
    people."""
    jobs = ((situation or {}).get("coordinator") or {}).get("jobs_completed_since_summary", 0)
    ideas = ((situation or {}).get("ideas_from_people") or {}).get("open", 0)
    said = [REVIEW_PROMPT.format(minutes=minutes), SUMMARY_STALE.format(jobs=jobs) if jobs else "",
            IDEAS_OPEN.format(ideas=ideas) if ideas else ""]
    return " ".join(part for part in said if part)


async def drain(env: dict[str, str], command: list[str] | None = None) -> list[dict]:
    """The events already waiting on the cursor, without waiting for more, and that an outage
    has ended. A drain runs after every tool call, so one that fails in an outage is not logged."""
    command = command or watch_command(env)
    code, stdout, _ = await _run_hl([*command, "--no-wait", "--timeout", str(_DRAIN_S)], env, lambda: False,
                                    warn=not outage.under_way())
    if code != 0:
        outage.suspect()
    return await _with_job_state([*_events(stdout), *outage.over()], env, command[0])


async def _wakes(env: dict[str, str], hl: str) -> tuple[str, ...] | None:
    """What ends the wait of whoever watches with env, as its platform experiment stands now.
    None for a coordinator whose platform experiment could not be read."""
    if _agent(env):
        return WAKES["agent"]
    code, stdout, _ = await _run_hl([hl, "platform-experiments", "get"], env, lambda: False)
    if code != 0:
        return None
    return WAKES[config.COORDINATOR] + (OPEN_WAKES if json.loads(stdout)["status"] == platform.PLATFORM_EXPERIMENT_OPEN else ())


async def _poll(env: dict[str, str], command: list[str], stopped: Callable[[], bool]) -> tuple[int | None, str]:
    """One poll of the platform: the wait for an event that wakes. A wait that could not be set
    up is a poll that failed."""
    wakes = await _wakes(env, command[0])
    if wakes is None:
        return 1, ""
    code, stdout, _ = await _run_hl([*command, "--wake", ",".join(wakes)], env, stopped)
    return code, stdout


async def next_batch(env: dict[str, str], stopped: Callable[[], bool], idle_s: float,
                     command: list[str] | None = None, retry_s: float = _RETRY_S, coalesce_s: float = COALESCE_S) -> list[dict]:
    """Runs hl watch until an event that wakes this role arrives, and returns it with what waited
    before it, and for a coordinator with what arrived in the coalesce_s after it. Once idle_s
    passes without one, returns what waits, which may be nothing. Returns [] as soon as stopped()
    is true. A failed hl watch is retried after retry_s. In an outage nothing is polled, and its
    end ends the wait with what waits and the platform reachable again."""
    command = command or watch_command(env)
    give_up = time.monotonic() + idle_s
    while not stopped() and time.monotonic() < give_up:
        if outage.under_way():
            await asyncio.sleep(min(1.0, retry_s))
            continue
        if again := outage.over():
            return [*await drain(env, command), *again]
        code, stdout = await _poll(env, command, stopped)
        events = await _with_job_state(_events(stdout), env, command[0])
        if events and not _agent(env):
            held_until = time.monotonic() + coalesce_s
            while not stopped() and (left := held_until - time.monotonic()) > 0:
                await asyncio.sleep(min(1.0, left))
            events += await drain(env, command) if not stopped() else []
        if events:
            return events
        if code not in (0, None):
            outage.suspect()
            deadline = min(give_up, time.monotonic() + retry_s)
            while not stopped() and time.monotonic() < deadline:
                await asyncio.sleep(min(1.0, retry_s))
    return [] if stopped() else await drain(env, command)


def event_line(event: dict, agent_id: str) -> str:
    """One compact line per event: kind, subject, value, detail, the machine an ended job ran on,
    and who did it when not the agent. The detail of a queue reason is its sentence, and that of
    a job evicted, requeued or failed its reason, as a code or as code: sentence."""
    if event.get("kind") == MESSAGE:
        to = "" if event.get("agent_id") == agent_id else f" to {event.get('subject', '')}"
        return f"message{to} from the {event.get('value', '')}: {event.get('detail', '')}"
    if event.get("kind") == RESET:
        return f"updates were reset, {event.get('detail', '')}"
    if event.get("kind") == GOAL:
        return f"the owner changed the goal (version {event.get('value', '')}): read it with `hl platform-experiments get`"
    parts = [event.get("kind", ""), event.get("subject", ""), event.get("value", ""), event.get("detail", "")]
    if event.get("machine"):
        parts.append(f"on {event['machine']}")
    actor = event.get("agent_id", "")
    if actor and actor != agent_id:
        parts.append(f"by {actor}")
    return " ".join(part for part in parts if part)


def message(events: list[dict], agent_id: str) -> str:
    return "Platform updates:\n" + "\n".join(dict.fromkeys(event_line(e, agent_id) for e in events))

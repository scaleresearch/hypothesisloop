"""Platform updates about the agent's own resources, delivered by the harness.

The harness runs `hl watch` whenever the agent ends its turn, and briefly after each of its tool
calls: the platform holds whatever happened while the agent worked and replays it from the cursor
hl saved, so a batch waits server-side instead of in this process, survives a restart, and is
delivered once whichever of the two hands it over, which is when its cursor moves. An event is a
pointer; a job that ended is handed over with the machine it ran on and its metrics, and to its
agent with the count of its jobs still unfinished and the quota it has used, read once per batch,
so that learning of it costs no call.
"""
from __future__ import annotations

import asyncio
import json
import logging
import os
import shutil
import time
from collections.abc import Callable

from hypothesisloop_agent import config, outage

log = logging.getLogger("llm_agent")

HL_WATCH = ["hl", "watch"]
_RETRY_S = 30.0
# A drain gives up after this long on a platform that does not answer.
_DRAIN_S = 5

# The kinds that wake whoever ended its turn, per role, and the one place that says so. An event
# of another kind stays on the platform (hl watch --wake) and is handed over with the next that
# wakes, after the next tool call, or once IDLE_NUDGE_S has passed. A coordinator has to learn
# within seconds that an agent finished or has gone idle (agent.idle: no job of its own
# in flight for the platform's notice, sent to that agent too), that a job is held by what waiting will not clear or has
# waited past its own estimate (job.held, sent once, to the job's agent too), that a stage
# advanced, that someone wrote a comment, that the brief or the baseline changed (a raised
# baseline is the bar every researcher is set against from then on), that the run closed or that its
# status_reason began to wait for the coordinator (platform_experiment.status, sent again with
# the reason as its detail, as when the last researcher's machine stops for good). An agent has
# to learn the same of its own jobs ending, and at once of a message the coordinator or the owner
# sent it (agent.message), which wakes no coordinator: its stream echoes the ones it sent. An agent
# that built an environment ahead of its job learns its build was loaded on a cluster, where its
# jobs run before the upload ends, or ended (environment.status) the same way: told to end its
# turn while it built, nothing else would wake it.
# Everything else is read when next acting. A kind wakes only for what its reader can act on.
WAKES = {
    "coordinator": ("agent.finished", "agent.idle", "experiment.blocked", "job.held", "stage.boundary", "comment.new",
                    "platform_experiment.status", "platform_experiment.description", "platform_experiment.baseline"),
    "agent": ("experiment.status", "experiment.blocked", "job.held", "job.underperforming", "agent.cut", "agent.idle",
              "agent.message", "stage.boundary", "comment.new", "platform_experiment.status", "platform_experiment.description",
              "platform_experiment.baseline", "platform_experiment.revision", "environment.status"),
}
# A message to one agent from the coordinator or the owner, told as what it says.
MESSAGE = "agent.message"
# A person asked a draft's planner to change its answer. The event is a pointer: what was asked is
# read from the draft (core.revising), and whoever hands a batch over says it in place of the event.
REVISION = "platform_experiment.revision"
# A build an agent asked for, loaded on a cluster or ended. Its subject is the environment's key,
# which says nothing to the agent: the detail names the commit and image it asked with.
ENVIRONMENT = "environment.status"
# What wakes a coordinator besides, while its platform experiment is open: a job that failed then
# is a baseline agent's, and one that failed for what the brief says is the coordinator's to
# repair. Three failed in a row before a coordinator heard of any, from the agent giving up. One
# that completed is acted on when its agent finishes, which wakes by itself.
OPEN_WAKES = ("experiment.status=FAILED",)

# How long an agent that ended its turn waits without any update before it is nudged: one that
# ended its turn with nothing in flight would otherwise wait for an update that never comes. Each
# nudge is a model call over the whole context, so the wait doubles with every nudge in a row, up
# to IDLE_NUDGE_MAX_S, and an update brings it back.
IDLE_NUDGE_S = 1800
IDLE_NUDGE_MAX_S = 4 * IDLE_NUDGE_S
IDLE_PROMPT = "No platform updates for {minutes} minutes. Continue with the most useful work available, if there is any."
# Said at once to an agent that ends its turn with no job of its own queued or running: it would
# otherwise wait until IDLE_NUDGE_S or the platform's agent.idle for an update that cannot come.
NOTHING_IN_FLIGHT = ("No job of yours is queued or running, so no update of your own is coming. A job submitted "
                     "while its environment builds waits for that build on the platform.")


def watch_env(api_url: str, token: str, statedir: str) -> dict[str, str]:
    """The environment hl watch needs: the API, the token, which says to hl who is watching, and
    a state dir that outlives the process."""
    return {**os.environ, "API_URL": api_url, "HL_TOKEN": token,
            "HL_STATE_DIR": os.path.join(os.path.abspath(statedir), ".hl-state")}


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
    """The agent the token in env is, or "" for a coordinator's."""
    return config.token_scope(env["HL_TOKEN"])[2]


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


_UNFINISHED = ("QUEUED", "SUBMITTED", "RUNNING")
_ENDED = ("COMPLETED", "FAILED", "EVICTED", "REJECTED", "CANCELLED")
# What `hl quota` says on stderr, before the platform's message, when it is refused with the
# reason quota_not_granted while the platform experiment is open: quota is granted at the start,
# so there is none to read and nothing failed.
_NO_QUOTA_YET = "hl: quota: quota_not_granted: "


def _machine(job: dict) -> str:
    """Where job ran, short: the physical machine of a rented instance, else the node, then the
    accelerator's model or, without one, the CPU's. `hl job list` prints the same as ran_on."""
    ran_on = job.get("ran_on")
    if not ran_on:
        return ""
    machine = ran_on.get("machine") or {}
    where = f"host {machine['host_id']}" if machine.get("host_id") else f"node {ran_on['node']}"
    return " ".join(part for part in (where, machine.get("accelerator_model") or machine.get("cpu_model")) if part)


def _result(job: dict) -> str:
    return ", ".join([
        *(f"{m['name']} {m['value']:.6g}" + ("" if m["basis"] == "raw" else f" ({m['basis']})")
          for m in job.get("metrics", [])),
        *([f"ranks nowhere: {job['not_rankable_reason']}"] if job.get("rankable") is False else [])])


async def _with_job_state(events: list[dict], env: dict[str, str], hl: str) -> list[dict]:
    """events, completed with what whoever is told of an ended job would otherwise read: the
    machine it ran on, and for a COMPLETED one its declared metrics and, when it ranks nowhere,
    why, as its event's detail. An agent's batch closes with two more events, the count of its
    unfinished jobs and its quota used. One hl call each per batch. A call that failed adds the
    event that says what was not read and the command that reads it: told COMPLETED with no
    metric and nothing else, an agent took the job for one that reported none."""
    def ended(e: dict) -> bool:
        return e["kind"] == "experiment.status" and e["value"] in _ENDED

    not_read = []

    async def read(what: str, *command: str) -> list[dict]:
        code, stdout, said = await _run_hl([hl, *command], env, lambda: False, warn=False)
        if code == 0:
            return _events(stdout)
        if not said.startswith(_NO_QUOTA_YET):
            _warn([hl, *command], code, said)
            not_read.append({"kind": "not_read", "value": f"{what}: `{' '.join(['hl', *command])}` reads it"})
        return []

    ids = list(dict.fromkeys(e["subject"] for e in events if ended(e)))
    if not ids:
        return events
    jobs = {job["id"]: job for job in await read("the metrics and machine of the jobs that ended", "job", "status", *ids)}

    def told(e: dict) -> dict:
        job = jobs.get(e["subject"]) if ended(e) else None
        if job is None:
            return e
        return dict(e, machine=_machine(job), **({"detail": _result(job)} if e["value"] == "COMPLETED" else {}))

    events = [told(e) for e in events]
    agent = _agent(env)
    if not agent:
        return events + not_read
    for stats in await read("your unfinished jobs", *_unfinished_read(agent)):
        events.append({"kind": "unfinished_jobs",
                       "value": ", ".join(f"{n} {s}" for s, n in _unfinished(stats).items()) or "none"})
    for q in await read("your quota", "quota"):
        events.append({"kind": "quota_used", "value": (
            f"{q['used_guaranteed_acch'] + q['used_burst_acch']:.3g} of {q['share_accelerator_hours']:g} AccH, "
            f"room for jobs holding {q['remaining_guaranteed_accelerator_hours']:.3g} guaranteed "
            f"or {q['remaining_burst_accelerator_hours']:.3g} burst at once" + _next_stage(q))})
    return events + not_read


def _unfinished_read(agent: str) -> list[str]:
    return ["job", "stats", "--agent", agent]


def _unfinished(stats: dict) -> dict[str, int]:
    """The agent's job counts by unfinished status, those it has none of left out."""
    return {s: stats["by_status"][s] for s in _UNFINISHED if stats["by_status"].get(s)}


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


def _next_stage(quota: dict) -> str:
    """What a later stage raises the share to, and when: a researcher finished with its share
    spent three minutes before the next stage more than doubled it."""
    if "next_stage_starts_at" not in quota:
        return ""
    return (f", and the share rises to {quota['next_stage_share_accelerator_hours']:.3g} AccH "
            f"by {quota['next_stage_starts_at']}")


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
    return WAKES["coordinator"] + (OPEN_WAKES if json.loads(stdout)["status"] == "open" else ())


async def _poll(env: dict[str, str], command: list[str], stopped: Callable[[], bool]) -> tuple[int | None, str]:
    """One poll of the platform: the wait for an event that wakes. A wait that could not be set
    up is a poll that failed."""
    wakes = await _wakes(env, command[0])
    if wakes is None:
        return 1, ""
    code, stdout, _ = await _run_hl([*command, "--wake", ",".join(wakes)], env, stopped)
    return code, stdout


async def next_batch(env: dict[str, str], stopped: Callable[[], bool], idle_s: float,
                     command: list[str] | None = None, retry_s: float = _RETRY_S) -> list[dict]:
    """Runs hl watch until an event that wakes this role arrives, and returns it with what waited
    before it. Once idle_s passes without one, returns what waits, which may be nothing. Returns
    [] as soon as stopped() is true. A failed hl watch is retried after retry_s. In an outage
    nothing is polled, and its end ends the wait with what waits and the platform reachable again."""
    command = command or watch_command(env)
    give_up = time.monotonic() + idle_s
    while not stopped() and time.monotonic() < give_up:
        if outage.under_way():
            await asyncio.sleep(min(1.0, retry_s))
            continue
        if again := outage.over():
            return [*await drain(env, command), *again]
        code, stdout = await _poll(env, command, stopped)
        events = _events(stdout)
        if events:
            return await _with_job_state(events, env, command[0])
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
    if event.get("kind") == ENVIRONMENT:
        return f"environment {event.get('value', '')}: {event.get('detail', '')}"
    parts = [event.get("kind", ""), event.get("subject", ""), event.get("value", ""), event.get("detail", "")]
    if event.get("machine"):
        parts.append(f"on {event['machine']}")
    actor = event.get("agent_id", "")
    if actor and actor != agent_id:
        parts.append(f"by {actor}")
    return " ".join(part for part in parts if part)


def message(events: list[dict], agent_id: str) -> str:
    return "Platform updates:\n" + "\n".join(dict.fromkeys(event_line(e, agent_id) for e in events))

from __future__ import annotations

import asyncio
import logging
import os
import signal
import subprocess
from dataclasses import dataclass
from datetime import datetime, timezone
from pathlib import Path

from hypothesisloop_agent import api_client, config, instructions, outage, platform, prompt

log = logging.getLogger("llm_agent")

# What a wait refused is answered with, as every built-in prompt says it once. Agents ended turns
# with `true` or an `echo` and waited for a job in sleep loops around `hl job status`: ending the
# turn is the one way to wait.
WAIT = "To wait, reply with no command in it: your turn ends, and the next update starts another."


def install_signal_handler() -> list:
    stop_flag = [False]

    def _handler(signum, _frame):
        log.info("received signal %s, stopping", signum)
        stop_flag[0] = True

    signal.signal(signal.SIGTERM, _handler)
    signal.signal(signal.SIGINT, _handler)
    return stop_flag


@dataclass
class RunSetup:
    """Everything the run loop needs, assembled once from the environment."""
    cfg: config.Config
    client: api_client.PlatformClient
    # hl's listing of its commands for the role, which fills {{hl_commands}} of every prompt the
    # platform serves it: the hl on this machine does not change while it runs.
    commands: str
    workdir: str
    # Where the harness keeps what it needs to go on: outside workdir, which holds nothing but
    # what the agent puts there, so a clone into it is clean and no file of the harness is ever
    # an untracked file of the agent's repository.
    statedir: str


def git_identity(cfg: config.Config) -> dict[str, str]:
    """The environment that makes every commit of the agent carry its id as author and
    committer, so a history that holds several agents' work says who wrote what. The image's
    own identity is one name for all of them."""
    name = f"coordinator-{cfg.platform_experiment_id}" if config.coordinates(cfg) else cfg.agent_id
    email = f"{name}@agents.hypothesisloop.invalid"
    return {"GIT_AUTHOR_NAME": name, "GIT_COMMITTER_NAME": name, "GIT_AUTHOR_EMAIL": email, "GIT_COMMITTER_EMAIL": email}


def own_python() -> None:
    """Puts the agent's virtualenv (AGENT_VENV, made with pip in the image) first on PATH for
    everything this process starts. uv run put the harness's own there, which has no pip: an
    agent's `pip install` went to a site the python3 on its PATH never read."""
    venv = config.required("AGENT_VENV", "the virtualenv the agent installs into")
    os.environ["VIRTUAL_ENV"] = venv
    os.environ["PATH"] = os.path.join(venv, "bin") + os.pathsep + os.environ["PATH"]


def git_through_platform(repository_url: str, git_url: str) -> None:
    """Sends every git this process starts, the agent's included, to the platform's git proxy for
    the repository, through the environment and no file: url.<git_url>.insteadOf rewrites the
    repository's address, and HL_GIT_URL is the one address the image's `hl git-credential`
    answers, with the agent's own token. The agent holds no credential of the git host, and the
    proxy refuses a push to any ref but the agent's own branches. The origin a clone records stays
    repository_url, which a job's code_ref names."""
    n = int(os.environ.get("GIT_CONFIG_COUNT", "0"))
    os.environ[f"GIT_CONFIG_KEY_{n}"] = f"url.{git_url}.insteadOf"
    os.environ[f"GIT_CONFIG_VALUE_{n}"] = repository_url
    os.environ["GIT_CONFIG_COUNT"] = str(n + 1)
    os.environ["HL_GIT_URL"] = git_url


_staging: list[subprocess.Popen] = []
_STAGE_TIMEOUT_S = 24 * 3600


def start_staging(setup: RunSetup) -> None:
    """Starts `hl env stage` in the background for an agent with jobs, so that `hl env run` has the
    image its jobs run in without the agent waiting for it. It waits for an environment still being
    built or pushed for as long as a session can last, and `hl env run` waits for it in turn and
    says why when it failed. Its output is in env-stage.log of the state directory.
    A Fly machine's /dev/fuse is root's alone, and `hl env run` mounts its views of the image
    through it as a user, which a distribution's default allows."""
    if not config.submits_jobs(setup.cfg):
        return
    if os.path.exists("/dev/fuse"):
        subprocess.run(["sudo", "chmod", "0666", "/dev/fuse"], check=True)
    with open(os.path.join(setup.statedir, "env-stage.log"), "ab") as out:
        _staging.append(subprocess.Popen(["hl", "env", "stage", "--timeout", str(_STAGE_TIMEOUT_S)], stdin=subprocess.DEVNULL, stdout=out,
                                         stderr=subprocess.STDOUT, cwd=setup.statedir, start_new_session=True))


_CLONE_TIMEOUT_S = 600


def clone_workspace(repository_url: str, branch: str, workdir: str) -> None:
    """Clones branch of the repository into an empty workdir before the CLI starts in it. The CLI caches
    that a directory is no git repository the first time it looks, for as long as it runs: an
    agent that cloned in its first turn had every `isolation: "worktree"` subagent refused with
    "not in a git repository". A clone that fails leaves workdir empty for the agent to clone."""
    if any(Path(workdir).iterdir()):
        return
    try:
        done = subprocess.run(["git", "clone", "--branch", branch, repository_url, "."], cwd=workdir, text=True, capture_output=True,
                              timeout=_CLONE_TIMEOUT_S)
    except (OSError, subprocess.TimeoutExpired) as e:
        log.warning("cloning %s into %s failed: %s", repository_url, workdir, e)
        return
    if done.returncode != 0:
        log.warning("cloning %s into %s exited %d: %s", repository_url, workdir, done.returncode,
                    (done.stderr or done.stdout).strip()[-500:])


async def prepare() -> RunSetup:
    """Shared startup: logging, config, the agent's Python, workdir, platform client, and the
    instructions it runs with, read once here for its role and to fail before the first
    connection on any it could not run."""
    logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(message)s")

    cfg = config.Config()
    own_python()

    client = api_client.PlatformClient(cfg.api_url, cfg.token)
    if config.coordinates(cfg):
        platform_experiment = await client.require_coordinated(cfg.platform_experiment_id)
    else:
        await client.require_joined(cfg.agent_id, cfg.platform_experiment_id)
        platform_experiment = await client.get_platform_experiment(cfg.platform_experiment_id)
    answer = await instructions.read(client, cfg)
    cfg.run_as(answer.get("role", ""))

    workdir = os.path.abspath(config.required("AGENT_WORKDIR", "the directory the agent clones into and works in"))
    statedir = config.required("AGENT_STATE_DIR", "the directory the harness keeps its own state in, outside AGENT_WORKDIR")
    os.makedirs(workdir, exist_ok=True)
    os.makedirs(statedir, exist_ok=True)

    # The repository is the one the platform experiment names, so no address and no git token is
    # handed to the agent at launch.
    repository_url = platform_experiment.get("repository_url")
    if not repository_url:
        raise SystemExit(f"agent: platform experiment {cfg.platform_experiment_id} names no repository, and an agent works in its clone of one")
    git_url = platform_experiment.get("git_url")
    if not git_url:
        raise SystemExit(f"agent: platform experiment {cfg.platform_experiment_id} names no git_url, the platform's git proxy its repository is reached through")
    git_through_platform(repository_url, git_url)
    branch = platform_experiment.get("branch")
    if not branch:
        raise SystemExit(f"agent: platform experiment {cfg.platform_experiment_id} names no branch, the one an agent clones")

    if not config.plans(cfg) and not config.coordinates(cfg):
        await asyncio.to_thread(clone_workspace, repository_url, branch, workdir)
    commands = await asyncio.to_thread(prompt.hl_commands, cfg)
    instructions.complete(answer, cfg, commands, workdir)
    return RunSetup(cfg=cfg, client=client, commands=commands, workdir=workdir, statedir=statedir)


async def revising(setup: RunSetup) -> dict | None:
    """The draft of a planner that has a changed goal to answer, None for anyone else and for a
    draft with none: the platform reads such a draft as revising, and as proposed or declined once
    the planner answered the goal it has."""
    if not config.plans(setup.cfg):
        return None
    draft = await setup.client.get_platform_experiment(setup.cfg.platform_experiment_id)
    if draft.get("status_reason", {}).get("reason") != platform.STATUS_REASON_REVISING:
        return None
    return draft


async def baseline_clock(setup: RunSetup) -> str:
    """What a baseline agent is told of its deadline with every prompt: baseline_due_at and the
    minutes left until it, as the platform has it now. "" for any other agent and once the
    experiment runs, when it has no such deadline."""
    if not config.measures_baseline(setup.cfg):
        return ""
    try:
        due = (await setup.client.get_platform_experiment(setup.cfg.platform_experiment_id)).get("baseline_due_at")
    except api_client.APIError as e:
        log.warning("could not read baseline_due_at (%s)", e)
        return "baseline_due_at could not be read: the platform did not answer."
    if not due:
        return ""
    left = (datetime.fromisoformat(due) - datetime.now(timezone.utc)).total_seconds() // 60
    return f"baseline_due_at is {due}, {max(0, int(left))} minutes from now."


# A baseline agent is signed up with the planner's proposal, so that its machine starts while the
# draft counts down to being accepted. It has nothing to measure before that.
ACCEPT_POLL_S = 3


async def until_accepted(setup: RunSetup, stop_flag: list, poll_s: float = ACCEPT_POLL_S, said=lambda: None) -> None:
    """Returns once the platform experiment of a baseline agent is no longer a draft, or a stop is
    asked for, and at once for any other agent. A platform that does not answer is waited out.
    said() is called once, when it starts waiting."""
    if not config.measures_baseline(setup.cfg):
        return
    waiting = False
    while not stop_flag[0]:
        try:
            status = (await setup.client.get_platform_experiment(setup.cfg.platform_experiment_id)).get("status")
        except api_client.APIError as e:
            log.warning("could not read whether the draft was accepted (%s)", e)
            status = None
        if status is not None and status != platform.PLATFORM_EXPERIMENT_DRAFT:
            return
        if not waiting:
            log.info("waiting for the draft to be accepted before measuring its baseline")
            said()
            waiting = True
        await asyncio.sleep(poll_s)


async def repo_map(setup: RunSetup) -> str:
    """What the planner recorded of the repository, "" where it recorded nothing or it could not
    be read."""
    try:
        proposal = (await setup.client.get_platform_experiment(setup.cfg.platform_experiment_id)).get("proposal") or {}
    except api_client.APIError as e:
        log.warning("could not read the planner's map of the repository (%s)", e)
        return ""
    return (proposal.get("repo_map") or "").strip()


def _closer(actor: dict | None) -> str:
    kind, name = (actor or {}).get("kind"), (actor or {}).get("name") or "unnamed"
    if kind == "coordinator":
        return "its coordinator closed it"
    if kind == "user":
        return f"a person closed it ({name})"
    if kind == "key":
        return f"it was closed with an API key ({name})"
    if kind == "staff":
        return "platform staff closed it"
    return "the platform does not say who closed it"


async def run_ended(setup: RunSetup) -> str:
    """Why the platform ended this agent's run, "" while it goes on. The platform decides it, and
    tells an agent sandbox to stop for this and for nothing else: closed, cut, finished, or
    planned for a planner whose answer to its draft is stored. A coordinator's run ends when its
    platform experiment closes."""
    return (await _run_end(setup))[0]


async def _run_end(setup: RunSetup) -> tuple[str, str]:
    """run_ended, and for a cut its cut_reason: "" for a cut at a stage boundary."""
    cfg = setup.cfg
    if config.coordinates(cfg):
        pe = await setup.client.get_platform_experiment(cfg.platform_experiment_id)
        return platform.RUN_END_CLOSED if pe["status"] == platform.PLATFORM_EXPERIMENT_CLOSED else "", ""
    signup = await setup.client.get_signup(cfg.agent_id, cfg.platform_experiment_id)
    return signup.get("ended", ""), signup.get("cut_reason", "")


def _cut_because(reason: str) -> str:
    if reason == "":
        return "You are being stopped because you were cut from the platform experiment at a stage boundary."
    if reason == platform.CUT_MAX_AGENTS_LOWERED:
        return ("You are being stopped because you were cut from the platform experiment: its max_agents was lowered "
                "below the researchers at work, and the newest of them were cut.")
    return f"You are being stopped because you were cut from the platform experiment, for a reason the platform names {reason}."


async def _closed_because(setup: RunSetup) -> str | None:
    cfg = setup.cfg
    pe = await setup.client.get_platform_experiment(cfg.platform_experiment_id)
    closed_at = pe["closed_at"]
    if pe.get("close_reason") == platform.CLOSE_END_TIME_REACHED:
        return f"You are being stopped because the platform experiment reached its end time and the platform closed it at {closed_at}."
    if pe.get("close_reason") == platform.CLOSE_PLANNING_LIMIT_REACHED:
        return f"You are being stopped because the planning limit passed with nothing proposed, and the platform closed the draft at {closed_at}."
    if pe.get("close_reason") == platform.CLOSE_BASELINE_LIMIT_REACHED:
        return f"You are being stopped because the platform experiment was not running by its baseline limit, and the platform closed it at {closed_at} before it started."
    if pe.get("close_reason") == platform.CLOSE_PROPOSAL_EXPIRED:
        return "You are being stopped because nobody took up your answer in time, and the platform closed the draft."
    if pe.get("close_reason") == platform.CLOSE_CLOSED_BY_REQUEST:
        closed_by = pe.get("closed_by")
        if config.coordinates(cfg) and (closed_by or {}).get("kind") == "coordinator":
            return None
        said = f" Their reason: {pe['close_message']}" if pe.get("close_message") else " They gave no reason."
        return (f"You are being stopped because the platform experiment was closed before its end time: "
                f"{_closer(closed_by)} at {closed_at}.{said}")
    return f"You are being stopped because the platform experiment closed at {closed_at}. The platform recorded no reason."


async def stop_cause(setup: RunSetup) -> str | None:
    """The sentence that tells the agent why its machine is being stopped, as the platform has it
    at this moment. None when the agent's own action ended its run: a researcher that finished, a
    planner whose answer is stored, a coordinator that closed its platform experiment. It is told
    nothing and stops by itself. A cause the platform does not give is said to be unknown, never
    inferred."""
    try:
        ended, cut_reason = await _run_end(setup)
        if ended == platform.RUN_END_CLOSED:
            return await _closed_because(setup)
        if ended == platform.RUN_END_CUT:
            return _cut_because(cut_reason)
        return {
            platform.RUN_END_FINISHED: None,
            platform.RUN_END_PLANNED: None,
            "": ("You are being stopped while your run on the platform goes on: the platform ended nothing of yours. "
                 "Only this machine is being stopped, and the platform does not say why."),
        }[ended]
    except api_client.APIError as e:
        log.warning("could not read why the machine stops (%s)", e)
        return "You are being stopped. The platform could not be asked why."


_RUN_CHECK_INTERVAL_S = 30


async def run_over(setup: RunSetup) -> str | None:
    """Why this process has no run on the platform to work in, None while it has one: the one
    place the harness asks, and so what starts and ends the outage clock (outage.py). The
    platform ended the run, or it refuses the token, which it does once the wind-down after the
    end has passed. A platform that does not answer is waited out."""
    try:
        # A planner's run ends with its answer and a change of the goal starts it again with its
        # session.
        ended = await run_ended(setup)
    except api_client.APIError as e:
        if e.status == 401:
            return f"the platform refuses this token, so its run is over ({e.body.strip()[:200]})"
        if not outage.under_way():
            log.warning("the platform did not say whether the run goes on (%s): waiting", e)
        outage.unanswered()
        return None
    outage.answered()
    return _ENDED + ended if ended else None


_ENDED = "the platform ended the run: "


def lost_platform(reason: str | None) -> bool:
    """Whether reason, of run_over, leaves nothing to ask the platform or file with it."""
    return bool(reason) and not reason.startswith(_ENDED)


async def until_lost(setup: RunSetup, stop_flag: list) -> str:
    """Returns once the platform refuses this process, with the reason. Whoever awaits it ends the process then, whatever turn or wait is in flight.
    A run the platform ended asks for the stop a signal asks for: its agent may still have a last
    turn to file what it owes, and only the wind-down its token has left to take it in."""
    while True:
        reason = await run_over(setup)
        if lost_platform(reason):
            return reason
        if reason and not stop_flag[0]:
            log.info("%s: stopping", reason)
            stop_flag[0] = True
        await outage.next_read(_RUN_CHECK_INTERVAL_S)


async def run_until_lost(setup: RunSetup, work, stop_flag: list) -> None:
    """Awaits work, the run loop, and exits the process as soon as the platform is lost:
    its turn in flight is dropped and no last turn follows, since both need the platform."""
    work, lost = asyncio.ensure_future(work), asyncio.ensure_future(until_lost(setup, stop_flag))
    try:
        await asyncio.wait({work, lost}, return_when=asyncio.FIRST_COMPLETED)
        if work.done():
            return work.result()
        work.cancel()
        await asyncio.gather(work, return_exceptions=True)
        raise SystemExit(f"agent: exiting: {lost.result()}")
    finally:
        work.cancel()
        lost.cancel()


async def stop_reason(setup: RunSetup, stop_flag: list) -> str | None:
    """Why the run loop stops, None while it goes on: run_over, or a signal."""
    return await run_over(setup) or ("received signal" if stop_flag[0] else None)

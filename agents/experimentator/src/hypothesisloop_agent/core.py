from __future__ import annotations

import json
import logging
import os
import signal
import subprocess
import time
from dataclasses import dataclass
from pathlib import Path

from hypothesisloop_agent import api_client, config

log = logging.getLogger("llm_agent")

# One briefing for every backend and flavor. It names hl's commands and repeats neither the
# platform's rules nor hl's command listing: both are spliced in at startup, the rules from the
# platform as {rules} and the listing from the hl on this PATH as {hl_commands}, so neither is a
# copy that can drift. The specialization lives in prompts/flavors/<flavor>.md, spliced in as
# {flavor_brief}.
_PROMPTS_DIR = Path(__file__).parent / "prompts"
_HL_HELP = ([], ["job"], ["hypothesis"])
# A coordinator changes the roster and the brief, which no agent does.
# A planner proposes or declines and runs nothing. A baseline agent writes the first job spec.
_HL_HELP_PLANNER = ([], ["platform-experiments", "propose"], ["platform-experiments", "decline"])
_HL_HELP_BASELINE = ([], ["job"], ["job", "submit"], ["hypothesis"], ["repo"])
_HL_HELP_COORDINATOR = ([], ["platform-experiments", "agents"], ["platform-experiments", "agents", "add"],
                        ["platform-experiments", "update"], ["platform-experiments", "summary"],
                        ["platform-experiments", "close"], ["job"], ["hypothesis"])


def hl_commands(cfg: config.Config) -> str:
    """hl's own listing of its commands and of the subcommands the briefing leans on."""
    groups = _HL_HELP
    if config.coordinates(cfg):
        groups = _HL_HELP_COORDINATOR
    elif config.plans(cfg):
        groups = _HL_HELP_PLANNER
    elif config.measures_baseline(cfg):
        groups = _HL_HELP_BASELINE
    # hl prints the help of a group on stdout and the help of one command on stderr.
    return "\n\n".join(
        subprocess.run(["hl", *group, "--help"], check=True, text=True,
                       stdout=subprocess.PIPE, stderr=subprocess.STDOUT).stdout.strip()
        for group in groups)


def _code_sections() -> dict[str, str]:
    """The paragraphs of a briefing about the experiment's repository. A baseline agent starts
    the code and a researcher starts from it, so how the workspace begins is a section per role."""
    text = (_PROMPTS_DIR / "code.md").read_text()
    sections = {}
    for section in text.split("\n==== ")[1:]:
        name, _, body = section.partition("\n")
        sections[name.strip()] = body.rstrip("\n")
    return sections


def system_prompt(cfg: config.Config, rules: str, commands: str, repository_url: str, workdir: str) -> str:
    values = dict(rules=rules.strip(), hl_commands=commands, platform_experiment_id=cfg.platform_experiment_id,
                  repository_url=repository_url, workdir=workdir)
    code = _code_sections()
    template, sections = "coordinator.md", ("repository_line", "start_code")
    if config.plans(cfg):
        template, sections = "planner.md", ()
        values.update(agent_id=cfg.agent_id)
    elif config.measures_baseline(cfg):
        template, sections = "baseline.md", ("workspace", "code_submit", "code_access")
        values.update(agent_id=cfg.agent_id)
        values.update(start=code["start_baseline"].format(**values))
    elif not config.coordinates(cfg):
        template, sections = "system_prompt.md", ("workspace", "code_submit", "code_access")
        values.update(agent_id=cfg.agent_id, flavor=cfg.flavor, hyperparameters=json.dumps(cfg.hyperparameters),
                      flavor_brief=(_PROMPTS_DIR / "flavors" / f"{cfg.flavor}.md").read_text().strip())
        values.update(start=code["start_researcher"].format(**values))
    return (_PROMPTS_DIR / template).read_text().format(**values, **{name: code[name].format(**values) for name in sections})


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
    """Everything a backend's run loop needs, assembled once from the environment."""
    cfg: config.Config
    client: api_client.PlatformClient
    system_prompt: str
    workdir: str
    # Where the harness keeps what it needs to go on: outside workdir, which holds nothing but
    # what the agent puts there, so a clone into it is clean and no file of the harness is ever
    # an untracked file of the agent's repository.
    statedir: str


def git_identity(cfg: config.Config) -> dict[str, str]:
    """The environment that makes every commit of the agent carry its id as author and
    committer, so a branch several agents push to, and a history read later, says who wrote
    what. The image's own identity is one name for all of them."""
    name = f"coordinator-{cfg.platform_experiment_id}" if config.coordinates(cfg) else cfg.agent_id
    email = f"{name}@agents.hypothesisloop.invalid"
    return {"GIT_AUTHOR_NAME": name, "GIT_COMMITTER_NAME": name, "GIT_AUTHOR_EMAIL": email, "GIT_COMMITTER_EMAIL": email}


def own_python() -> None:
    """Puts the agent's virtualenv (AGENT_VENV, made with pip in the image) first on PATH for
    everything this process starts. uv run put the harness's own there, which has no pip: an
    agent's `pip install` went to a site the python3 on its PATH never read."""
    venv = os.environ.get("AGENT_VENV")
    if not venv:
        raise SystemExit("agent: missing required env var: AGENT_VENV, the virtualenv the agent installs into")
    os.environ["VIRTUAL_ENV"] = venv
    os.environ["PATH"] = os.path.join(venv, "bin") + os.pathsep + os.environ["PATH"]


def prepare() -> RunSetup:
    """Shared startup: logging, config, the agent's Python, workdir, platform client, prompt."""
    logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(message)s")

    cfg = config.Config()
    own_python()

    client = api_client.PlatformClient(cfg.api_url, cfg.token)
    if config.coordinates(cfg):
        platform_experiment = client.require_coordinated(cfg.platform_experiment_id)
        cfg.run_as(config.COORDINATOR_FLAVOR, (platform_experiment.get("coordinator") or {}).get("model"))
    else:
        signup = client.require_joined(cfg.agent_id, cfg.platform_experiment_id)
        cfg.run_as(signup.get("flavor"), signup.get("model"), signup.get("hyperparameters"), signup.get("role"))
        platform_experiment = client.get_platform_experiment(cfg.platform_experiment_id)

    # Per-agent by default: two agents launched from the same cwd (the natural way to run a
    # multi-agent competition on one box) must not resolve to the same repo checkout.
    workdir = os.environ.get("AGENT_WORKDIR", f"./workspace/{cfg.agent_id}")
    statedir = os.environ.get("AGENT_STATE_DIR", f"./state/{cfg.agent_id}")
    os.makedirs(workdir, exist_ok=True)
    os.makedirs(statedir, exist_ok=True)

    # The repository is the one the platform experiment names: the agent clones it through
    # `hl git-credential`, so no address and no git token is handed to it at launch.
    repository_url = platform_experiment.get("repository_url")
    if not repository_url:
        raise SystemExit(f"agent: platform experiment {cfg.platform_experiment_id} names no repository, and an agent works in its clone of one")

    commands = hl_commands(cfg)
    system_prompt_text = system_prompt(cfg, client.fetch_rules(), commands, repository_url, os.path.abspath(workdir))
    return RunSetup(cfg=cfg, client=client, system_prompt=system_prompt_text, workdir=workdir, statedir=statedir)


def revision_asked(setup: RunSetup) -> str | None:
    """What the person last asked a planner to change, None for anyone else and for a draft
    nobody revised. A planner's run ends with its answer, so a planner started with a session is
    one whose answer met a revision it has not answered yet."""
    if not config.plans(setup.cfg):
        return None
    revisions = setup.client.get_platform_experiment(setup.cfg.platform_experiment_id).get("revisions")
    return revisions[-1]["message"] if revisions else None


def stop_requested(setup: RunSetup, started_at: float, stop_flag: list) -> bool:
    """The stop conditions that need no platform call: a signal or MAX_WALL_HOURS."""
    hours = setup.cfg.max_wall_hours
    return stop_flag[0] or bool(hours and (time.time() - started_at) / 3600.0 >= hours)


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


def run_ended(setup: RunSetup) -> str:
    """Why the platform ended this agent's run, "" while it goes on. The platform decides it, and
    tells a hosted machine to stop for this and for nothing else: closed, cut, finished, or
    planned for a planner whose answer to its draft is stored. A coordinator's run ends when its
    platform experiment closes."""
    cfg = setup.cfg
    if config.coordinates(cfg):
        return "closed" if setup.client.get_platform_experiment(cfg.platform_experiment_id)["status"] == "closed" else ""
    return setup.client.get_signup(cfg.agent_id, cfg.platform_experiment_id).get("ended", "")


def _closed_because(setup: RunSetup) -> str | None:
    cfg = setup.cfg
    pe = setup.client.get_platform_experiment(cfg.platform_experiment_id)
    closed_at = pe.get("closed_at", "an unrecorded time")
    if pe.get("close_reason") == "end_time_reached":
        return f"You are being stopped because the platform experiment reached its end time, {pe['ends_at']}, and the platform closed it."
    if pe.get("close_reason") == "planning_limit_reached":
        return f"You are being stopped because the planning limit, {pe['planning_ends_at']}, passed with nothing proposed, and the platform closed the draft."
    if pe.get("close_reason") == "baseline_limit_reached":
        return "You are being stopped because no baseline was declared inside the baseline limit, and the platform closed the platform experiment before it started."
    if pe.get("close_reason") == "proposal_expired":
        return "You are being stopped because nobody took up your answer in time, and the platform closed the draft."
    if pe.get("close_reason") == "closed_by_request":
        closed_by = pe.get("closed_by")
        if config.coordinates(cfg) and (closed_by or {}).get("kind") == "coordinator":
            return None
        return (f"You are being stopped because the platform experiment was closed before its end time: "
                f"{_closer(closed_by)} at {closed_at}.")
    return f"You are being stopped because the platform experiment closed at {closed_at}. The platform recorded no reason."


def stop_cause(setup: RunSetup) -> str | None:
    """The sentence that tells the agent why its machine is being stopped, as the platform has it
    at this moment. None when the agent's own action ended its run: a researcher that finished, a
    planner whose answer is stored, a coordinator that closed its platform experiment. It is told
    nothing and stops by itself. A cause the platform does not give is said to be unknown, never
    inferred."""
    try:
        ended = run_ended(setup)
        if ended == "closed":
            return _closed_because(setup)
        return {
            "finished": None,
            "planned": None,
            "cut": "You are being stopped because you were cut from the platform experiment at a stage boundary.",
            "": ("You are being stopped while your run on the platform goes on: the platform ended nothing of yours. "
                 "Only this machine is being stopped, and the platform does not say why."),
        }[ended]
    except api_client.APIError as e:
        log.warning("could not read why the machine stops (%s)", e)
        return "You are being stopped. The platform could not be asked why."


def stop_reason(setup: RunSetup, started_at: float, stop_flag: list) -> str | None:
    """Between-event stop check shared by both backends. Returns a reason string, or None."""
    if stop_flag[0]:
        return "received signal"
    cfg = setup.cfg
    if cfg.max_wall_hours and (time.time() - started_at) / 3600.0 >= cfg.max_wall_hours:
        return f"reached MAX_WALL_HOURS={cfg.max_wall_hours}"
    try:
        # A planner's run ends with its answer and a revision starts it again with its session.
        ended = run_ended(setup)
        if ended:
            return f"the platform ended the run: {ended}"
    except api_client.APIError as e:
        log.warning("could not check platform experiment status (%s) — continuing", e)
    return None

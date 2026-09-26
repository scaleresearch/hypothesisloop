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
# A planner pushes the seed code and proposes, which no other agent does.
_HL_HELP_PLANNER = ([], ["job"], ["hypothesis"], ["code"], ["platform-experiments", "propose"], ["platform-experiments", "decline"])
_HL_HELP_COORDINATOR = ([], ["platform-experiments", "agents"], ["platform-experiments", "agents", "add"],
                        ["platform-experiments", "update"], ["platform-experiments", "summary"],
                        ["platform-experiments", "close"], ["job"], ["hypothesis"])


def hl_commands(coordinates: bool = False, plans: bool = False) -> str:
    """hl's own listing of its commands and of the subcommands the briefing leans on."""
    groups = _HL_HELP_COORDINATOR if coordinates else _HL_HELP_PLANNER if plans else _HL_HELP
    return "\n\n".join(
        subprocess.run(["hl", *group, "--help"], check=True, capture_output=True, text=True).stdout.strip()
        for group in groups)


def _code_sections(repository_url: str) -> dict[str, str]:
    """The paragraphs of a briefing that depend on where code lives: the experiment's repository,
    or with none the platform, which keeps each pushed commit."""
    text = (_PROMPTS_DIR / "code" / ("repository.md" if repository_url else "platform.md")).read_text()
    sections = {}
    for section in text.split("\n==== ")[1:]:
        name, _, body = section.partition("\n")
        sections[name.strip()] = body.rstrip("\n")
    return sections


def system_prompt(cfg: config.Config, rules: str, commands: str, repository_url: str) -> str:
    values = dict(rules=rules.strip(), hl_commands=commands, platform_experiment_id=cfg.platform_experiment_id,
                  repository_url=repository_url)
    template, sections = "coordinator.md", ("repository_line", "start_code")
    if config.plans(cfg):
        template, sections = "planner.md", ("code_submit", "code_access")
        values.update(agent_id=cfg.agent_id)
    elif not config.coordinates(cfg):
        template, sections = "system_prompt.md", ("workspace", "code_submit", "code_access")
        values.update(agent_id=cfg.agent_id, flavor=cfg.flavor, hyperparameters=json.dumps(cfg.hyperparameters),
                      flavor_brief=(_PROMPTS_DIR / "flavors" / f"{cfg.flavor}.md").read_text().strip())
    code = _code_sections(repository_url)
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
    # Empty for a platform experiment that names no repository.
    repository_url: str = ""


def git_identity(cfg: config.Config) -> dict[str, str]:
    """The environment that makes every commit of the agent carry its id as author and
    committer, so a branch several agents push to, and a history read later, says who wrote
    what. The image's own identity is one name for all of them."""
    name = f"coordinator-{cfg.platform_experiment_id}" if config.coordinates(cfg) else cfg.agent_id
    email = f"{name}@agents.hypothesisloop.invalid"
    return {"GIT_AUTHOR_NAME": name, "GIT_COMMITTER_NAME": name, "GIT_AUTHOR_EMAIL": email, "GIT_COMMITTER_EMAIL": email}


def prepare() -> RunSetup:
    """Shared startup: logging, config, workdir, platform client, prompt."""
    logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(message)s")

    cfg = config.Config()

    client = api_client.PlatformClient(cfg.api_url, cfg.token)
    if config.coordinates(cfg):
        platform_experiment = client.require_coordinated(cfg.platform_experiment_id)
        cfg.run_as(config.COORDINATOR_FLAVOR, (platform_experiment.get("coordinator") or {}).get("model"))
    else:
        signup = client.require_joined(cfg.agent_id, cfg.platform_experiment_id)
        cfg.run_as(signup.get("flavor"), signup.get("model"), signup.get("hyperparameters"))
        platform_experiment = client.get_platform_experiment(cfg.platform_experiment_id)

    # hl reads who it is from HL_TOKEN. These two are what its commands that have not moved to
    # the token yet still read, so they say what the token says and nothing left over from
    # whoever started this process.
    os.environ["PLATFORM_EXPERIMENT_ID"] = cfg.platform_experiment_id
    os.environ.pop("AGENT_ID", None)
    if not config.coordinates(cfg):
        os.environ["AGENT_ID"] = cfg.agent_id

    # Per-agent by default: two agents launched from the same cwd (the natural way to run a
    # multi-agent competition on one box) must not resolve to the same repo checkout.
    workdir = os.environ.get("AGENT_WORKDIR", f"./workspace/{cfg.agent_id}")
    os.makedirs(workdir, exist_ok=True)

    # The repository is the one the platform experiment names: the agent clones it through
    # `hl git-credential`, so no address and no git token is handed to it at launch. With none
    # named, the agent starts in an empty directory and the platform keeps what it pushes.
    repository_url = platform_experiment.get("repository_url") or ""

    commands = hl_commands(plans=True) if config.plans(cfg) else hl_commands(config.coordinates(cfg))
    system_prompt_text = system_prompt(cfg, client.fetch_rules(), commands, repository_url)
    return RunSetup(cfg=cfg, client=client, system_prompt=system_prompt_text, workdir=workdir, repository_url=repository_url)


def stop_requested(setup: RunSetup, started_at: float, stop_flag: list) -> bool:
    """The stop conditions that need no platform call: a signal or MAX_WALL_HOURS."""
    hours = setup.cfg.max_wall_hours
    return stop_flag[0] or bool(hours and (time.time() - started_at) / 3600.0 >= hours)


def stop_reason(setup: RunSetup, started_at: float, stop_flag: list) -> str | None:
    """Between-event stop check shared by both backends. Returns a reason string, or None."""
    if stop_flag[0]:
        return "received signal"
    cfg = setup.cfg
    if cfg.max_wall_hours and (time.time() - started_at) / 3600.0 >= cfg.max_wall_hours:
        return f"reached MAX_WALL_HOURS={cfg.max_wall_hours}"
    try:
        pe = setup.client.get_platform_experiment(cfg.platform_experiment_id)
        if pe["status"] == "closed":
            return "platform experiment closed"
        # A planner's work is the proposal, or the reason there is none: once either is stored
        # (both set proposed_at), or the draft was accepted, the
        # platform releases its machine, and a revision starts it again with its session.
        if config.plans(cfg) and (pe["status"] != "draft" or pe.get("proposed_at")):
            return "the draft is answered"
    except api_client.APIError as e:
        log.warning("could not check platform experiment status (%s) — continuing", e)
    return None

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


def hl_commands() -> str:
    """hl's own listing of its commands and of the subcommands of job and hypothesis."""
    return "\n\n".join(
        subprocess.run(["hl", *group, "--help"], check=True, capture_output=True, text=True).stdout.strip()
        for group in _HL_HELP)


def system_prompt(cfg: config.Config, rules: str, commands: str, code_repo_url: str) -> str:
    return (_PROMPTS_DIR / "system_prompt.md").read_text().format(
        rules=rules.strip(), hl_commands=commands, agent_id=cfg.agent_id, flavor=cfg.flavor,
        flavor_brief=(_PROMPTS_DIR / "flavors" / f"{cfg.flavor}.md").read_text().strip(),
        platform_experiment_id=cfg.platform_experiment_id, code_repo_url=code_repo_url,
        hyperparameters=json.dumps(cfg.hyperparameters),
    )


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


def prepare() -> RunSetup:
    """Shared startup: logging, config, workdir, platform client, prompt."""
    logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(message)s")

    cfg = config.Config()

    # Per-agent by default: two agents launched from the same cwd (the natural way to run a
    # multi-agent competition on one box) must not resolve to the same repo checkout. agent_id is
    # required and validated, so the default is always unique with no extra config.
    workdir = os.environ.get("AGENT_WORKDIR", f"./workspace/{cfg.agent_id}")
    os.makedirs(workdir, exist_ok=True)

    client = api_client.PlatformClient(cfg.api_url, cfg.token)
    client.require_joined(cfg.agent_id, cfg.platform_experiment_id)

    # The repository is the project's, attached on the platform: the agent clones it through
    # `hl git-credential`, so no address and no git token is handed to it at launch.
    code_repo_url = client.get_platform_experiment(cfg.platform_experiment_id).get("code_repo_url")
    if not code_repo_url:
        raise SystemExit(f"agent: the project of platform experiment {cfg.platform_experiment_id} has no repository "
                         "attached: attach one to the project, then start the agent again")

    system_prompt_text = system_prompt(cfg, client.fetch_rules(), hl_commands(), code_repo_url)
    return RunSetup(cfg=cfg, client=client, system_prompt=system_prompt_text, workdir=workdir)


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
    except api_client.APIError as e:
        log.warning("could not check platform experiment status (%s) — continuing", e)
    return None

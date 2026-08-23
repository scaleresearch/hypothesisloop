from __future__ import annotations

import logging
import os
import signal
import time
from dataclasses import dataclass
from pathlib import Path

from dotenv import load_dotenv

from hypothesisloop_agent import api_client, config

log = logging.getLogger("llm_agent")

# Backend-agnostic by construction: the prompt names no tool and prescribes no mechanism for
# reaching the platform. Each harness already knows its own tool set, and every backend gets a
# byte-identical prompt — so a result difference between backends is the model, not the briefing.
#
# API-agnostic the same way, and this one is load-bearing: the prompt names capabilities, never
# endpoints. The live /explore digests spliced in as {api_guide} are the only place a path,
# method or query parameter appears, so an endpoint that is renamed, split or added reaches every
# agent the moment the service serves it. Every path written here instead would be a second,
# silently-drifting copy of a contract this file cannot see.
#
# One briefing for every role. The differentiation that matters between a competitor, a baseline
# and a reviewer is what each is asked to do, not what the platform lets it do — every role's jobs
# are admitted, billed and settled identically. So the capability content lives here once and the
# role delta lives in prompts/roles/<role>.md, which the agent reads itself at $ROLE_BRIEFS: three
# parallel prompts drifted apart within hours of the first capability being added to one of them.
_PROMPTS_DIR = Path(__file__).parent / "prompts"


def install_signal_handler() -> list:
    stop_flag = [False]

    def _handler(signum, _frame):
        log.info("received signal %s — will stop after the current message finishes", signum)
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
    max_turns: int
    workdir: str


def prepare() -> RunSetup:
    """Shared startup: logging, .env, config, workdir, platform client, live API guide, prompt."""
    logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(message)s")
    load_dotenv(".env")

    cfg = config.Config()
    cfg.validate()
    max_turns = int(os.environ.get("MAX_LLM_TURNS", "0") or 0)  # 0 = unlimited

    # Per-agent by default: two agents launched from the same cwd (the natural way to run a
    # multi-agent competition on one box) must not resolve to the same repo checkout. agent_id is
    # required and validated, so the default is always unique with no extra config.
    workdir = os.environ.get("AGENT_WORKDIR", f"./workspace/{cfg.agent_id}")
    os.makedirs(workdir, exist_ok=True)

    # Deliberately no register_agent()/signup() calls here — those are platform operations like
    # any other, described in the live API guide, and the agent decides for itself when/whether
    # to do them. client is also used for the run loop's own stop-condition check (platform
    # experiment closed), not for taking actions on the agent's behalf.
    client = api_client.PlatformClient(cfg.api_url)

    # The API contract, fetched live from the running services rather than checked in anywhere:
    # this is the prompt's only source of endpoints (see system_prompt.md), so it is
    # required, not decorative — fetch_api_guide raises rather than briefing an agent without it.
    api_guide = client.fetch_api_guide()

    template = (_PROMPTS_DIR / "system_prompt.md").read_text()
    system_prompt = template.format(
        api_guide=api_guide, agent_id=cfg.agent_id, role=cfg.role,
        platform_experiment_id=cfg.platform_experiment_id, code_repo_url=cfg.code_repo_url,
    )
    return RunSetup(cfg=cfg, client=client, system_prompt=system_prompt,
                    max_turns=max_turns, workdir=workdir)


def stop_reason(setup: RunSetup, started_at: float, stop_flag: list) -> str | None:
    """Between-event stop check shared by both backends. Returns a reason string, or None."""
    if stop_flag[0]:
        return "received signal"
    cfg = setup.cfg
    if cfg.max_wall_hours and (time.time() - started_at) / 3600.0 >= cfg.max_wall_hours:
        return f"reached MAX_WALL_HOURS={cfg.max_wall_hours}"
    try:
        pe = setup.client.get_platform_experiment(cfg.platform_experiment_id)
        if pe.get("status") == "closed":
            return "platform experiment closed"
    except api_client.APIError as e:
        log.warning("could not check platform experiment status (%s) — continuing", e)
    return None

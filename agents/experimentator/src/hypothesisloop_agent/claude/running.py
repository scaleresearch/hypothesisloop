"""The instructions the session runs with, and applying new ones at a turn boundary."""
from __future__ import annotations

import logging

from hypothesisloop_agent import api_client, core, instructions
from hypothesisloop_agent.claude import rotation, stream, transcripts
from hypothesisloop_agent.claude.store import Store

log = logging.getLogger("llm_agent")

# Opens a session restarted with a new system prompt.
PROMPT_REPLACED = ("The instructions you run with changed: the system prompt you read now is their new version, "
                   "and it replaces the one this session ran with so far.")


async def _set_effort(client, effort: str) -> None:
    """Changes how hard the model thinks from the next call on, in the session. The SDK has no
    method for the CLI's apply_flag_settings."""
    await client._query._send_control_request({"subtype": "apply_flag_settings", "settings": {"effortLevel": effort}}, timeout=20)


class Running:
    def __init__(self, setup: core.RunSetup):
        self._setup = setup
        # The instructions the session runs with: its system prompt and model, and the subagent
        # limits and effort, which change in the session. Set at the start of each connection.
        self.current: instructions.Instructions | None = None
        # Whether agent.instructions said the agent's instructions changed since they were last
        # read: they are read and applied at the next turn boundary.
        self.told = False

    async def _fetch(self) -> instructions.Instructions:
        setup = self._setup
        return await instructions.fetch(setup.client, setup.cfg, setup.commands, setup.workdir)

    def not_own(self, events: list[dict]) -> list[dict]:
        """events without the ones that say the agent's own instructions changed, which are noted."""
        rest = [e for e in events if not instructions.addressed(e, self._setup.cfg)]
        if len(rest) < len(events):
            self.told = True
        return rest

    async def connect(self, resume_id: str | None) -> bool:
        """Reads the instructions a connection starts with from the platform, and says whether
        they replace the system prompt of the session it resumes. A resumed session whose recorded
        prompt alone differs is resumed with the new one; otherwise it is run with the instructions
        it was started with and the ones read now are applied at its first turn boundary."""
        statedir = self._setup.statedir
        newest = await self._fetch()
        kept = instructions.load(statedir) if resume_id else None
        replaced = bool(kept and kept.needs_new_prompt(newest) and not kept.needs_new_session(newest)
                        and transcripts.records_prompt(resume_id))
        if replaced:
            transcripts.drop_recorded_prompt(resume_id)
            log.info("resuming session %s with the new system prompt of %s version %d (digest %s)",
                     resume_id, newest.agent_type, newest.version, newest.digest[:12])
        running = self.current = newest if replaced else kept or newest
        self.told = running.digest != newest.digest
        instructions.save(statedir, running)
        stream.progress.instructions(running.digest, running.version)
        log.info("running %s version %d (digest %s): model=%s effort=%s max_subagents=%d%s",
                 running.agent_type, running.version, running.digest[:12], running.model, running.effort,
                 running.max_subagents,
                 f", version {newest.version} (digest {newest.digest[:12]}) applies at the first turn boundary" if self.told else "")
        return replaced

    async def apply(self, client, store: Store, background: dict[str, dict], resume_id: str | None) -> bool:
        """At a turn boundary, reads the instructions agent.instructions said changed and applies
        them: the subagent limits and the effort in the session, a prompt by ending the connection
        for the next to resume the session with it (connect), a model in a fresh session the lead
        hands off to. So is a prompt whose session's transcript records none to replace. A subagent
        that ended before the boundary has reported; one still at work stops with the session, and
        the handoff names it for the lead to start again, or a resumed session says so by itself.
        Whether the connection ended for the next to start. A read or an apply that failed is tried
        again at the next boundary."""
        if not self.told:
            return False
        try:
            newest = await self._fetch()
        except api_client.APIError as e:
            log.warning("the changed instructions were not read (%s): reading them at the next turn boundary", e)
            return False
        self.told = False
        running = self.current
        if newest.digest == running.digest:
            return False
        if running.needs_new_prompt(newest) and not running.needs_new_session(newest):
            if transcripts.records_prompt(resume_id):
                log.info("instructions changed to %s version %d (digest %s), its prompt with them: resuming the session with it",
                         newest.agent_type, newest.version, newest.digest[:12])
                return True
            log.error("the transcript of session %s records no system prompt to replace: handing off to a fresh session", resume_id)
        if running.needs_new_session(newest) or running.needs_new_prompt(newest):
            log.info("instructions changed to %s version %d (digest %s), its prompt, model or effort with them: handing off to a fresh session",
                     newest.agent_type, newest.version, newest.digest[:12])
            if await rotation.hand_off(client, store, self._setup.cfg.agent_id, rotation.INSTRUCTIONS_ROTATION, background, resume_id):
                return True
            self.told = True
            return False
        if newest.effort != running.effort:
            try:
                await _set_effort(client, newest.effort)
            except Exception as e:
                log.warning("the effort was not changed to %s (%s): trying again at the next turn boundary", newest.effort, e)
                self.told = True
                return False
        log.info("instructions changed to %s version %d (digest %s) in the session: effort=%s max_subagents=%d",
                 newest.agent_type, newest.version, newest.digest[:12], newest.effort, newest.max_subagents)
        self.current = newest
        stream.progress.instructions(newest.digest, newest.version)
        instructions.save(self._setup.statedir, newest)
        return False

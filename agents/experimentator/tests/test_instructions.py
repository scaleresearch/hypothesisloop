import asyncio
import contextlib
import io
import os
import tempfile
import unittest
from types import SimpleNamespace
from unittest import mock

from claude_agent_sdk import ResultMessage, SystemMessage, UserMessage

import conftest
from conftest import served
from hypothesisloop_agent import api_client, config, instructions
from hypothesisloop_agent.claude import run
from test_require_joined import derived, served as answer
from test_turn_watchdog import S, HangingClient, StoppableClient, _setup

OWN = {"kind": "agent.instructions", "subject": "agent-a", "agent_id": "agent-a", "value": "2", "detail": "generalist"}


def _handoff():
    """The turn that answers the handoff's prompt, which the CLI says again as the turn starts."""
    return [UserMessage(content=run._INSTRUCTIONS_ROTATION), _result("where things stand")]


def _result(text="worked"):
    return ResultMessage(subtype="success", duration_ms=1, duration_api_ms=1, is_error=False, num_turns=1,
                         session_id="s-1", result=text)


def _config(scope: str) -> config.Config:
    with mock.patch.dict(os.environ, {"API_URL": "http://api", "HL_TOKEN": derived(scope)}, clear=True):
        return config.Config()


class FetchTest(unittest.TestCase):
    def fetch(self, cfg, served_answer):
        asked = []
        client = SimpleNamespace(get_json=lambda path: asked.append(path) or served_answer)
        return conftest.FETCH(client, cfg, "usage: hl", "/agent/workspace"), asked

    def test_an_agent_and_a_coordinator_read_their_own_instructions_filled_for_the_machine(self):
        agent = _config("agent:pe-1/ag-1")
        got, asked = self.fetch(agent, answer(role="planner"))
        self.assertEqual(asked, ["/platform-experiments/pe-1/agents/ag-1/instructions"])
        self.assertEqual(got, instructions.Instructions(
            agent_type="generalist", version=1, role="planner", prompt="Work in /agent/workspace with:\nusage: hl",
            model="model-1", max_subagents=4, pacing_subagents=2, effort="high", hyperparameters={"batch_size": 64},
            digest="digest-1"))
        self.assertEqual(agent.role, "planner")
        coordinator = _config("coordinator:pe-1")
        got, asked = self.fetch(coordinator, answer(role="coordinator"))
        self.assertEqual((asked, got.role), (["/platform-experiments/pe-1/coordinator/instructions"], "coordinator"))

    # A type is a role for good: instructions that named another would brief an agent for a part
    # its token cannot play.
    def test_instructions_of_another_role_end_the_harness(self):
        agent = _config("agent:pe-1/ag-1")
        agent.run_as("researcher")
        with self.assertRaises(SystemExit) as ctx:
            self.fetch(agent, answer(role="baseline"))
        self.assertIn("name the role 'baseline'", str(ctx.exception))

    def test_what_a_session_runs_with_outlives_the_machine(self):
        statedir = tempfile.mkdtemp()
        self.assertIsNone(instructions.load(statedir))
        kept = served(SimpleNamespace(role="researcher"), hyperparameters={"lr": 0.1})
        instructions.save(statedir, kept)
        self.assertEqual(instructions.load(statedir), kept)


def settings(client) -> list:
    """What the client was sent to change in the session: it is also asked for the account's usage."""
    return [c for c in client.controls if c["subtype"] != "get_usage"]


class InstructedClient(StoppableClient):
    """Ends each turn as the test lists them, with a control channel that records what it is sent."""

    turns: list = []

    def __init__(self, options):
        super().__init__(options)
        self.options = options
        self.controls = []

        async def control(request, timeout=0):
            self.controls.append(request)
            return {}

        self._query = SimpleNamespace(_send_control_request=control)

    async def receive_response(self):
        await asyncio.sleep(0)
        for message in (InstructedClient.turns.pop(0) if InstructedClient.turns else [_result()]):
            yield message


class SessionTest(unittest.IsolatedAsyncioTestCase):
    async def run_session(self, fetched: list, batches: list, turns: list, resume_id=None, setup=None):
        """One session whose fetches answer fetched in turn and whose waits return batches in turn,
        the last of which stops it."""
        stop_flag = [False]
        setup = setup or _setup()
        reads = list(fetched)

        def fetch(client, cfg, commands, workdir):
            self.assertEqual((commands, workdir), (setup.commands, setup.workdir))
            if isinstance(reads[0], Exception):
                raise reads.pop(0)
            return reads.pop(0) if len(reads) > 1 else reads[0]

        async def next_batch(env, stopped, idle_s):
            batch = batches.pop(0)
            if not batches:
                stop_flag[0] = True
            return batch

        InstructedClient.turns = list(turns)
        with mock.patch.object(run, "ClaudeSDKClient", InstructedClient), \
                mock.patch.object(instructions, "fetch", fetch), \
                mock.patch.object(run.updates, "next_batch", next_batch), \
                mock.patch.object(run.updates, "jobs_in_flight", mock.AsyncMock(return_value=1)), \
                mock.patch.object(run.updates, "drain", mock.AsyncMock(return_value=[])), \
                contextlib.redirect_stdout(io.StringIO()), self.assertLogs("llm_agent") as logged:
            rotated = await asyncio.wait_for(run._run_one_session(setup, resume_id, stop_flag), timeout=5)
        self.logged = "\n".join(logged.output)
        self.setup = setup
        return rotated, HangingClient.instances[-1]

    async def test_a_session_runs_with_the_prompt_model_effort_and_limits_it_is_served(self):
        setup = _setup()
        first = served(setup.cfg, prompt="be a researcher", model="claude-x", effort="max", max_subagents=7, pacing_subagents=3)
        _, client = await self.run_session([first], [[]], [], setup=setup)
        options = client.options
        self.assertEqual((options.system_prompt, options.model, options.effort),
                         ("be a researcher" + run._FILE_TOOLS_NOTE, "claude-x", "max"))
        self.assertEqual(run._subagent_cap(), (7, ""))
        self.assertEqual(instructions.load(setup.statedir), first)

    # An edit of the subagent limits or the effort reaches the agent at the next turn boundary, in
    # its session: nothing it does in flight stops for it.
    async def test_new_limits_and_effort_apply_in_the_session_at_the_turn_boundary(self):
        setup = _setup()
        first = served(setup.cfg)
        edited = served(setup.cfg, version=2, digest="digest-2", effort="low", max_subagents=6, pacing_subagents=1)
        comment = {"kind": "comment.new", "subject": "hyp-1", "value": "human"}
        rotated, client = await self.run_session([first, edited], [[OWN], [comment], []], setup=setup, turns=[])
        self.assertFalse(rotated)
        self.assertEqual(settings(client), [{"subtype": "apply_flag_settings", "settings": {"effortLevel": "low"}}])
        self.assertEqual(run._running, edited)
        self.assertEqual(run._subagent_cap(), (6, ""))
        self.assertEqual(instructions.load(setup.statedir), edited)
        # The event is the harness's: the agent is told nothing of it, and the wait goes on to the
        # next batch.
        self.assertEqual(client.queries[1], "Platform updates:\ncomment.new hyp-1 human" + S)
        self.assertNotIn("agent.instructions", " ".join(client.queries))

    # The CLI's effortLevel setting holds low to xhigh and drops max without a word, so a change to
    # or from max is a fresh session, and one between the others applies in the session.
    async def test_a_change_to_or_from_max_effort_hands_off_to_a_fresh_session(self):
        for before, after in (("high", "max"), ("max", "xhigh")):
            setup = _setup()
            first = served(setup.cfg, effort=before)
            edited = served(setup.cfg, version=2, digest="digest-2", effort=after)
            rotated, client = await self.run_session([first, edited], [[OWN], []], [[_result()], _handoff()], setup=setup)
            self.assertTrue(rotated, (before, after))
            self.assertEqual(settings(client), [])
            self.assertEqual(client.queries[1:], [run._INSTRUCTIONS_ROTATION])
        for before, after in (("high", "xhigh"), ("max", "max")):
            self.assertFalse(served(setup.cfg, effort=before).needs_new_session(served(setup.cfg, digest="d2", effort=after, max_subagents=5)))

    # A session keeps the system prompt it started with, a resumed one too (seen on the real API):
    # a new prompt or model starts a fresh session the lead hands off to, and a subagent still at
    # work stops with the old one and is named for the lead to start again.
    async def test_a_new_prompt_or_model_hands_off_to_a_fresh_session_naming_the_subagents_it_stops(self):
        for changed in ({"prompt": "the new prompt"}, {"model": "claude-y"}):
            setup = _setup()
            first = served(setup.cfg)
            edited = served(setup.cfg, version=2, digest="digest-2", **changed)
            subagents = SystemMessage("background_tasks_changed", {"tasks": [
                {"task_id": "sub-1", "task_type": "local_agent", "description": "lr warmup on hyp-1"}]})
            turns = [[subagents, _result()], _handoff()]
            rotated, client = await self.run_session([first, edited], [[OWN], []], turns, setup=setup)
            self.assertTrue(rotated, changed)
            self.assertEqual(client.queries[1:], [run._INSTRUCTIONS_ROTATION])
            self.assertEqual(run._load_handoff(setup), "where things stand\n\n" + run._STOPPED_SUBAGENTS.format(
                tasks="- lr warmup on hyp-1"))
            self.assertEqual(settings(client), [])
            # The next connection starts fresh, and runs what the platform serves then.
            self.assertEqual(run._restart_state(setup)[0], None)

    # The event can arrive while a turn is under way: the hook keeps it from the agent and the
    # change applies once the turn ends.
    async def test_an_event_handed_over_during_a_turn_applies_when_the_turn_ends(self):
        setup = _setup()
        first, edited = served(setup.cfg), served(setup.cfg, version=2, digest="digest-2", prompt="the new prompt")
        hook = run._updates_hook(setup, {"HL_STATE_DIR": os.path.join(setup.statedir, ".hl-state")}, asyncio.Lock())
        with mock.patch.object(run.updates, "drain", mock.AsyncMock(return_value=[OWN])), \
                mock.patch.object(run, "_running", first), contextlib.redirect_stdout(io.StringIO()):
            self.assertEqual(await hook({"hook_event_name": "PostToolUse"}, "tool-1", {}), {})
        self.assertTrue(run._instructions_told)
        with mock.patch.object(run, "_connection_instructions", lambda setup, resume_id: first):
            rotated, client = await self.run_session([edited], [[]], [[_result()], _handoff()], setup=setup)
        self.assertTrue(rotated)
        self.assertEqual(client.queries[1:], [run._INSTRUCTIONS_ROTATION])

    # A machine replaced while the type was edited resumes a session that still holds the old
    # prompt: it runs as it was started, and hands off at its first turn boundary.
    async def test_a_resumed_session_runs_with_what_it_was_started_with_until_its_first_boundary(self):
        setup = _setup()
        kept = served(setup.cfg, prompt="the old prompt", effort="medium")
        instructions.save(setup.statedir, kept)
        newest = served(setup.cfg, version=2, digest="digest-2", prompt="the new prompt")
        rotated, client = await self.run_session([newest], [[]], [[_result()], _handoff()],
                                                 resume_id="s-1", setup=setup)
        self.assertEqual((client.options.system_prompt, client.options.effort), ("the old prompt" + run._FILE_TOOLS_NOTE, "medium"))
        self.assertTrue(rotated)
        self.assertIn("version 2 (digest digest-2) applies at the first turn boundary", self.logged)

    async def test_a_read_that_fails_at_the_boundary_is_tried_again_at_the_next(self):
        setup = _setup()
        first = served(setup.cfg)
        edited = served(setup.cfg, version=2, digest="digest-2", max_subagents=9)
        away = api_client.APIError("GET", "/x", 503, "unavailable")
        comment = {"kind": "comment.new", "subject": "hyp-1", "value": "human"}
        rotated, client = await self.run_session([first, away, edited], [[OWN], [comment], []], [], setup=setup)
        self.assertFalse(rotated)
        self.assertIn("reading them at the next turn boundary", self.logged)
        self.assertEqual(run._running, edited)

    # A coordinator steers researchers whose type was edited, and its own type is its alone.
    async def test_a_coordinator_is_told_of_its_researchers_instructions_and_not_of_its_own(self):
        setup = _setup()
        setup.cfg.agent_id = setup.cfg.role = "coordinator"
        theirs = {"kind": "agent.instructions", "subject": "ag-7", "agent_id": "ag-7", "value": "2", "detail": "generalist"}
        own = {"kind": "agent.instructions", "subject": "coordinator", "agent_id": "coordinator", "value": "3", "detail": "coordinator"}
        with mock.patch.object(run, "_running", served(setup.cfg)):
            told = await run._told(setup, {}, [theirs, own])
        self.assertTrue(told.startswith("Platform updates:\nagent.instructions ag-7 2 generalist by ag-7"), told)
        self.assertNotIn("coordinator", told.split("\n\n")[0])
        self.assertTrue(run._instructions_told)

    async def test_the_instructions_a_session_runs_with_travel_with_it(self):
        setup = _setup()
        stored = []
        sessions = SimpleNamespace(save=lambda files, transcripts: stored.append(files))
        with mock.patch.object(run, "_sessions", sessions), mock.patch.dict(os.environ, {"CLAUDE_CONFIG_DIR": tempfile.mkdtemp()}):
            await run._store_session(setup)
        self.assertIn(instructions.FILE, stored[0])


class WakeTest(unittest.TestCase):
    def test_an_edit_wakes_an_agent_and_a_coordinator_waiting_between_turns(self):
        self.assertIn(instructions.EVENT, run.updates.WAKES["agent"])
        self.assertIn(instructions.EVENT, run.updates.WAKES["coordinator"])


if __name__ == "__main__":
    unittest.main()

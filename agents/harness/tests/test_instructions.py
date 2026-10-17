import asyncio
import contextlib
import io
import json
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


def _transcript(home: str, session_id="s-1", prompt="the system prompt") -> str:
    """A transcript as the CLI writes one: its prompt recorded before the turn that ran with it."""
    lines = [
        {"type": "user", "uuid": "u-1", "parentUuid": None, "sessionId": session_id,
         "message": {"role": "user", "content": "the secret number is 42"}},
        {"type": "attachment", "uuid": "snap-1", "parentUuid": "u-1", "sessionId": session_id,
         "attachment": {"type": "prompt_snapshot", "systemPrompt": [prompt]}},
        {"type": "attachment", "uuid": "snap-2", "parentUuid": "snap-1", "sessionId": session_id,
         "attachment": {"type": "prompt_snapshot", "systemPrompt": [prompt], "tools": []}},
        {"type": "assistant", "uuid": "a-1", "parentUuid": "snap-2", "sessionId": session_id,
         "message": {"role": "assistant", "content": [{"type": "thinking", "thinking": "noted", "signature": "sig"},
                                                      {"type": "text", "text": "OK"}]}},
        {"type": "last-prompt", "leafUuid": "a-1", "sessionId": session_id},
    ]
    path = os.path.join(home, "projects", "p", f"{session_id}.jsonl")
    os.makedirs(os.path.dirname(path), exist_ok=True)
    with open(path, "w") as f:
        f.writelines(json.dumps(line) + "\n" for line in lines)
    return path


def _lines(path: str) -> list[dict]:
    with open(path) as f:
        return [json.loads(line) for line in f]


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
    def setUp(self):
        self.home = tempfile.mkdtemp()
        patched = mock.patch.dict(os.environ, {"CLAUDE_CONFIG_DIR": self.home})
        patched.start()
        self.addCleanup(patched.stop)

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

    # A session keeps the model it started with: a new model starts a fresh session the lead hands
    # off to, and a subagent still at work stops with the old one and is named for the lead to start again.
    async def test_a_new_model_hands_off_to_a_fresh_session_naming_the_subagents_it_stops(self):
        setup = _setup()
        _transcript(self.home)
        first = served(setup.cfg)
        edited = served(setup.cfg, version=2, digest="digest-2", prompt="the new prompt", model="claude-y")
        subagents = SystemMessage("background_tasks_changed", {"tasks": [
            {"task_id": "sub-1", "task_type": "local_agent", "description": "lr warmup on hyp-1"}]})
        turns = [[subagents, _result()], _handoff()]
        rotated, client = await self.run_session([first, edited], [[OWN], []], turns, setup=setup)
        self.assertTrue(rotated)
        self.assertEqual(client.queries[1:], [run._INSTRUCTIONS_ROTATION])
        self.assertEqual(run._load_handoff(setup), "where things stand\n\n" + run._STOPPED_SUBAGENTS.format(
            tasks="- lr warmup on hyp-1"))
        self.assertEqual(settings(client), [])
        # The next connection starts fresh, and runs what the platform serves then.
        self.assertEqual(run._restart_state(setup)[0], None)

    # A new prompt on the same model ends the connection at the boundary, with no handoff, and the
    # next resumes the session, its whole history kept, with the new prompt in place of the one
    # its transcript recorded (seen on the real API).
    async def test_a_new_prompt_resumes_the_session_with_it_and_its_history(self):
        setup = _setup()
        transcript = _transcript(self.home)
        first = served(setup.cfg)
        edited = served(setup.cfg, version=2, digest="digest-2", prompt="the new prompt", effort="low")
        rotated, client = await self.run_session([first, edited], [[OWN], []], [[_result()]], setup=setup)
        self.assertTrue(rotated)
        self.assertEqual(client.queries[1:], [])
        self.assertIsNone(run._load_handoff(setup))
        self.assertEqual(run._restart_state(setup), ("s-1", None))
        self.assertEqual(len(_lines(transcript)), 5)

        rotated, client = await self.run_session([edited], [[]], [], resume_id="s-1", setup=setup)
        self.assertFalse(rotated)
        self.assertEqual((client.options.system_prompt, client.options.effort, client.options.resume),
                         ("the new prompt" + run._FILE_TOOLS_NOTE, "low", "s-1"))
        self.assertTrue(client.queries[0].startswith(run._PROMPT_REPLACED + "\n\nContinue where you left off."), client.queries[0])
        self.assertEqual(instructions.load(setup.statedir), edited)
        lines = _lines(transcript)
        self.assertEqual([line["uuid"] for line in lines if "uuid" in line], ["u-1", "a-1"])
        self.assertEqual(lines[1]["parentUuid"], "u-1")
        self.assertEqual(lines[1]["message"]["content"][0]["signature"], "sig")
        self.assertEqual(lines[2], {"type": "last-prompt", "leafUuid": "a-1", "sessionId": "s-1"})

    # A new current version of the agent's type reaches the running harness as agent.instructions:
    # the session is resumed in place, on the same machine, with the new prompt and its whole
    # history, and nothing of the agent's on the platform is touched, its jobs least of all: the
    # harness only reads.
    async def test_a_new_version_swaps_the_prompt_in_place_keeping_history_and_leaves_jobs_alone(self):
        setup = _setup()
        platform_calls = []

        def read(name):
            def call(*args):
                platform_calls.append(name)
                return {"status": "running"} if name == "get_platform_experiment" else {}
            return call

        setup.client = SimpleNamespace(get_platform_experiment=read("get_platform_experiment"), get_signup=read("get_signup"))
        transcript = _transcript(self.home)
        first = served(setup.cfg)
        edited = served(setup.cfg, version=2, digest="digest-2", prompt="the prompt of version 2")
        rotated, _ = await self.run_session([first, edited], [[OWN], []], [[_result()]], setup=setup)
        self.assertTrue(rotated)
        self.assertIsNone(run._load_handoff(setup))
        rotated, client = await self.run_session([edited], [[]], [], resume_id="s-1", setup=setup)
        self.assertFalse(rotated)
        self.assertEqual((client.options.system_prompt, client.options.resume), ("the prompt of version 2" + run._FILE_TOOLS_NOTE, "s-1"))
        self.assertEqual([line["uuid"] for line in _lines(transcript) if "uuid" in line], ["u-1", "a-1"])
        self.assertEqual(run._running.version, 2)
        self.assertTrue(set(platform_calls) <= {"get_platform_experiment", "get_signup"}, platform_calls)

    # A transcript that records no prompt cannot be resumed with another: a CLI that stopped
    # recording one would resume with whatever it keeps instead. That session is handed off.
    async def test_a_new_prompt_hands_off_when_the_transcript_records_none_to_replace(self):
        setup = _setup()
        first = served(setup.cfg)
        edited = served(setup.cfg, version=2, digest="digest-2", prompt="the new prompt")
        rotated, client = await self.run_session([first, edited], [[OWN], []], [[_result()], _handoff()], setup=setup)
        self.assertTrue(rotated)
        self.assertEqual(client.queries[1:], [run._INSTRUCTIONS_ROTATION])
        self.assertIn("the transcript of session s-1 records no system prompt to replace: handing off to a fresh session", self.logged)
        self.assertEqual(run._restart_state(setup)[0], None)

    # The event can arrive while a turn is under way: the hook keeps it from the agent and the
    # change applies once the turn ends.
    async def test_an_event_handed_over_during_a_turn_applies_when_the_turn_ends(self):
        setup = _setup()
        _transcript(self.home)
        first, edited = served(setup.cfg), served(setup.cfg, version=2, digest="digest-2", prompt="the new prompt")
        hook = run._updates_hook(setup, {"HL_STATE_DIR": os.path.join(setup.statedir, ".hl-state")}, asyncio.Lock())
        with mock.patch.object(run.updates, "drain", mock.AsyncMock(return_value=[OWN])), \
                mock.patch.object(run, "_running", first), contextlib.redirect_stdout(io.StringIO()):
            self.assertEqual(await hook({"hook_event_name": "PostToolUse"}, "tool-1", {}), {})
        self.assertTrue(run._instructions_told)
        with mock.patch.object(run, "_connection_instructions", lambda setup, resume_id: (first, False)):
            rotated, client = await self.run_session([edited], [[]], [[_result()]], setup=setup)
        self.assertTrue(rotated)
        self.assertEqual(client.queries[1:], [])
        self.assertIn("its prompt with them: resuming the session with it", self.logged)

    # A machine replaced while the type was edited resumes a session that still holds the old
    # prompt: a new prompt alone it resumes with at once; with a new model it runs as it was
    # started, and hands off at its first turn boundary.
    async def test_a_resumed_session_takes_a_new_prompt_and_keeps_its_model_until_its_first_boundary(self):
        setup = _setup()
        transcript = _transcript(self.home, prompt="the old prompt")
        instructions.save(setup.statedir, served(setup.cfg, prompt="the old prompt", effort="medium"))
        newest = served(setup.cfg, version=2, digest="digest-2", prompt="the new prompt")
        rotated, client = await self.run_session([newest], [[]], [], resume_id="s-1", setup=setup)
        self.assertFalse(rotated)
        self.assertEqual((client.options.system_prompt, client.options.effort), ("the new prompt" + run._FILE_TOOLS_NOTE, "high"))
        self.assertIn("resuming session s-1 with the new system prompt of generalist version 2", self.logged)
        self.assertFalse(any(line.get("type") == "attachment" for line in _lines(transcript)))

        setup = _setup()
        _transcript(self.home, prompt="the old prompt")
        instructions.save(setup.statedir, served(setup.cfg, prompt="the old prompt", effort="medium"))
        newest = served(setup.cfg, version=2, digest="digest-2", prompt="the new prompt", model="claude-y")
        rotated, client = await self.run_session([newest], [[]], [[_result()], _handoff()],
                                                 resume_id="s-1", setup=setup)
        self.assertEqual((client.options.system_prompt, client.options.effort), ("the old prompt" + run._FILE_TOOLS_NOTE, "medium"))
        self.assertTrue(rotated)
        self.assertEqual(client.queries[1:], [run._INSTRUCTIONS_ROTATION])
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
        # A coordinator wakes for its own edit only: its researchers' are read in the situation.
        self.assertIn(f"{instructions.EVENT}@{config.COORDINATOR}", run.updates.WAKES["coordinator"])
        self.assertNotIn(instructions.EVENT, run.updates.WAKES["coordinator"])


if __name__ == "__main__":
    unittest.main()

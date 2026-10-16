import asyncio
import contextlib
import io
import os
import signal
import tempfile
import unittest
from types import SimpleNamespace
from unittest import mock

from claude_agent_sdk import ResultMessage

from hypothesisloop_agent import config, core
from hypothesisloop_agent.claude import run
from test_require_joined import AGENT_TOKEN

# What a launcher hands a machine, and all of it: where the platform is and the agent's token.
LAUNCH_ENV = {"API_URL": "http://api.example.invalid", "HL_TOKEN": AGENT_TOKEN}


class LaunchEnvironmentTest(unittest.TestCase):
    def test_the_token_says_who_this_is_and_nothing_in_the_environment_says_what_it_runs_as(self):
        stale = {"AGENT_FLAVOR": "data-search", "AGENT_MODEL": "model-1", "ANTHROPIC_MODEL": "model-2", "AGENT_ID": "left-over"}
        with mock.patch.dict(os.environ, {**LAUNCH_ENV, **stale}, clear=True):
            cfg = config.Config()
        self.assertEqual((cfg.agent_id, cfg.platform_experiment_id, cfg.role), ("agent-a", "pe-1", ""))
        for role in ("", "competitor", "coordinator"):
            with self.assertRaises(SystemExit):
                cfg.run_as(role)
        cfg.run_as("planner")
        self.assertEqual(cfg.role, "planner")
        with self.assertRaises(SystemExit):
            cfg.run_as("researcher")


class StopSignalTest(unittest.TestCase):
    def test_sigterm_which_a_launcher_sends_an_ending_agent_requests_the_stop(self):
        before = {s: signal.getsignal(s) for s in (signal.SIGTERM, signal.SIGINT)}
        try:
            stop_flag = core.install_signal_handler()
            self.assertEqual(stop_flag, [False])
            os.kill(os.getpid(), signal.SIGTERM)
            self.assertEqual(stop_flag, [True])
        finally:
            for signum, handler in before.items():
                signal.signal(signum, handler)

    def test_the_last_turn_fits_inside_the_wind_down_with_a_launcher_pass_to_spare(self):
        # domain.AgentWindDown is 120 s: the token works and the machine stays that long after
        # the close or the cut, and the signal arrives one launcher pass into it.
        self.assertLessEqual(run._SHUTDOWN_TURN_S, 90)

    def test_the_coordinators_last_turn_fits_inside_its_own_wind_down(self):
        # domain.CoordinatorWindDown is 300 s: the coordinator writes the final summary in it.
        self.assertLessEqual(run._COORDINATOR_SHUTDOWN_TURN_S, 300 - 60)
        self.assertGreater(run._COORDINATOR_SHUTDOWN_TURN_S, run._SHUTDOWN_TURN_S)


class ErrorTurnClient:
    """Ends its one turn with the error text the test set."""

    result = ""
    status = None

    def __init__(self, options):
        pass

    async def __aenter__(self):
        return self

    async def __aexit__(self, *exc):
        return False

    async def query(self, prompt):
        pass

    async def interrupt(self):
        pass

    async def receive_response(self):
        yield ResultMessage(subtype="success", duration_ms=1, duration_api_ms=1, is_error=True, num_turns=1,
                            session_id="s-1", result=ErrorTurnClient.result, api_error_status=ErrorTurnClient.status)


class RefusedCredentialTest(unittest.IsolatedAsyncioTestCase):
    async def session(self, result: str, status: int | None = None):
        setup = SimpleNamespace(commands="usage: hl", workdir=tempfile.mkdtemp(), statedir=tempfile.mkdtemp(),
                                client=SimpleNamespace(get_signup=lambda *_: {}),
                                cfg=SimpleNamespace(agent_id="ag-1", role="researcher", api_url="http://x", token=AGENT_TOKEN, platform_experiment_id="pe-1"))
        with mock.patch.object(run, "ClaudeSDKClient", ErrorTurnClient), \
                mock.patch.object(ErrorTurnClient, "result", result), \
                mock.patch.object(ErrorTurnClient, "status", status), \
                contextlib.redirect_stdout(io.StringIO()):
            return await asyncio.wait_for(run._run_one_session(setup, None, [False]), timeout=5)

    async def test_a_refused_credential_ends_the_agent_with_the_providers_words(self):
        for refusal in ("Not logged in · Please run /login", "Invalid API key · Fix external API key",
                        'API Error: 401 {"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}',
                        "OAuth token has expired. Please obtain a new token"):
            with self.assertRaises(SystemExit) as ended:
                await self.session(refusal)
            self.assertIn("refused this agent's credential", str(ended.exception))
            self.assertIn(refusal[:20], str(ended.exception))

    async def test_an_error_that_can_pass_is_waited_out(self):
        for transient, status in (('API Error: 529 {"type":"error","error":{"type":"overloaded_error"}}', 529),
                                  ("API Error: 500 Internal server error", 500), ("Connection error.", None), ("", None),
                                  ("You've hit your usage limit", 429), ("API Error: 408 Request timeout", 408)):
            self.assertEqual(await self.session(transient, status), False, transient)

    # A coordinator on a model its CLI was too old for was asked again every few minutes for hours.
    async def test_a_request_the_api_refuses_for_good_ends_the_agent_with_the_providers_words(self):
        for refusal, status in (("API Error: 400 Claude Code 2.1.212 does not support this model; version 2.1.280 or "
                                 "newer is required.", 400),
                                ("There's an issue with the selected model (claude-x). It may not exist or you may not "
                                 "have access to it.", 404)):
            with self.assertRaises(SystemExit) as ended:
                await self.session(refusal, status)
            self.assertIn(f"refused this agent's requests ({status})", str(ended.exception))
            self.assertIn(refusal[:40], str(ended.exception))

    def test_a_context_that_overflowed_is_no_refused_request(self):
        overflow = ResultMessage(subtype="success", duration_ms=1, duration_api_ms=1, is_error=True, num_turns=1,
                                 session_id="s-1", result="Prompt is too long", api_error_status=400)
        self.assertFalse(run.request_refused(overflow))

    def test_only_the_providers_refusal_of_the_credential_counts(self):
        self.assertFalse(run.credential_refused(None))
        self.assertFalse(run.credential_refused("rate_limit_error: too many requests"))
        self.assertTrue(run.credential_refused("NOT LOGGED IN"))

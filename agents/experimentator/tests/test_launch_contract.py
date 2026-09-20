"""What the harness is started with and how it ends when that cannot work: the values a hosted
agent's launcher hands it, the signal the launcher sends when the agent's run is over, and a
credential the model provider refuses."""
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

LAUNCH_ENV = {
    "API_URL": "http://api.example.invalid", "AGENT_ID": "ag-1", "HL_TOKEN": "hls_token",
    "PLATFORM_EXPERIMENT_ID": "pe-1", "AGENT_FLAVOR": "data-search", "AGENT_MODEL": "model-1",
    "AGENT_HYPERPARAMETERS": '{"batch_size": 64}',
}


class LaunchEnvironmentTest(unittest.TestCase):
    def test_the_values_a_launcher_hands_over_are_the_whole_config(self):
        with mock.patch.dict(os.environ, LAUNCH_ENV, clear=True):
            cfg = config.Config()
        self.assertEqual((cfg.flavor, cfg.model, cfg.hyperparameters), ("data-search", "model-1", {"batch_size": 64}))

    def test_flavor_and_model_have_no_default(self):
        for name in ("AGENT_FLAVOR", "AGENT_MODEL"):
            env = {k: v for k, v in LAUNCH_ENV.items() if k != name}
            with mock.patch.dict(os.environ, env, clear=True), self.assertRaises(SystemExit) as refused:
                config.Config()
            self.assertIn(name, str(refused.exception))

    def test_the_model_once_read_from_another_variable_is_not_read(self):
        env = {k: v for k, v in LAUNCH_ENV.items() if k != "AGENT_MODEL"}
        env["ANTHROPIC_MODEL"] = "model-from-the-old-variable"
        with mock.patch.dict(os.environ, env, clear=True), self.assertRaises(SystemExit):
            config.Config()


class StopSignalTest(unittest.TestCase):
    def test_sigterm_which_a_launcher_sends_an_ending_agent_requests_the_stop(self):
        before = {s: signal.getsignal(s) for s in (signal.SIGTERM, signal.SIGINT)}
        try:
            stop_flag = core.install_signal_handler()
            setup = SimpleNamespace(cfg=SimpleNamespace(max_wall_hours=0))
            self.assertFalse(core.stop_requested(setup, 0.0, stop_flag))
            os.kill(os.getpid(), signal.SIGTERM)
            self.assertTrue(core.stop_requested(setup, 0.0, stop_flag))
        finally:
            for signum, handler in before.items():
                signal.signal(signum, handler)

    def test_the_last_turn_fits_inside_the_wind_down_with_a_launcher_pass_to_spare(self):
        # domain.AgentWindDown is 120 s: the token works and the machine stays that long after
        # the close or the cut, and the signal arrives one launcher pass into it.
        self.assertLessEqual(run._SHUTDOWN_TURN_S, 90)


class ErrorTurnClient:
    """Ends its one turn with the error text the test set."""

    result = ""

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
                            session_id="s-1", result=ErrorTurnClient.result)


class RefusedCredentialTest(unittest.IsolatedAsyncioTestCase):
    async def session(self, result: str):
        setup = SimpleNamespace(system_prompt="", workdir=tempfile.mkdtemp(),
                                cfg=SimpleNamespace(agent_id="ag-1", api_url="http://x", platform_experiment_id="pe-1",
                                                    max_wall_hours=0))
        with mock.patch.object(run, "ClaudeSDKClient", ErrorTurnClient), \
                mock.patch.object(ErrorTurnClient, "result", result), \
                contextlib.redirect_stdout(io.StringIO()):
            return await asyncio.wait_for(run._run_one_session(setup, "m", None, 0.0, [False]), timeout=5)

    async def test_a_refused_credential_ends_the_agent_with_the_providers_words(self):
        for refusal in ("Not logged in · Please run /login", "Invalid API key · Fix external API key",
                        'API Error: 401 {"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}',
                        "OAuth token has expired. Please obtain a new token"):
            with self.assertRaises(SystemExit) as ended:
                await self.session(refusal)
            self.assertIn("refused this agent's credential", str(ended.exception))
            self.assertIn(refusal[:20], str(ended.exception))

    async def test_an_error_that_can_pass_is_waited_out(self):
        for transient in ('API Error: 529 {"type":"error","error":{"type":"overloaded_error"}}',
                          "API Error: 500 Internal server error", "Connection error.", "", "You've hit your usage limit"):
            self.assertEqual(await self.session(transient), (None, False), transient)

    def test_only_the_providers_refusal_of_the_credential_counts(self):
        self.assertFalse(run.credential_refused(None))
        self.assertFalse(run.credential_refused("rate_limit_error: too many requests"))
        self.assertTrue(run.credential_refused("NOT LOGGED IN"))

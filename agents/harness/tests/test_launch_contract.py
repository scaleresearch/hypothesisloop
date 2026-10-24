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

from hypothesisloop_agent import config, core, instructions, platform
from conftest import FakePlatform, connect, served
from hypothesisloop_agent.claude import cli, connection, results, shutdown
from test_require_joined import AGENT_TOKEN

# What a launcher hands a machine, and all of it: where the platform is and the agent's token.
LAUNCH_ENV = {platform.ENV_API_URL: "http://api.example.invalid", platform.ENV_TOKEN: AGENT_TOKEN}


class LaunchEnvironmentTest(unittest.TestCase):
    def test_the_token_says_who_this_is_and_nothing_in_the_environment_says_what_it_runs_as(self):
        stale = {"AGENT_FLAVOR": "data-search", "AGENT_MODEL": "model-1", "ANTHROPIC_MODEL": "model-2", "AGENT_ID": "left-over"}
        with mock.patch.dict(os.environ, {**LAUNCH_ENV, **stale}, clear=True):
            cfg = config.Config()
        self.assertEqual((cfg.agent_id, cfg.platform_experiment_id, cfg.role), ("agent-a", "pe-1", ""))
        for role in ("", "reviewer", "coordinator"):
            with self.assertRaises(SystemExit):
                cfg.run_as(role)
        cfg.run_as("planner")
        self.assertEqual(cfg.role, "planner")
        with self.assertRaises(SystemExit):
            cfg.run_as("executor")
        for role in ("executor", "researcher", "baseline"):
            with mock.patch.dict(os.environ, LAUNCH_ENV, clear=True):
                agent = config.Config()
            agent.run_as(role)
            self.assertEqual(agent.role, role)


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
        # The token works and the machine stays the wind-down after the close or the cut, and the
        # signal arrives one launcher pass, 30 s, into it.
        self.assertLessEqual(shutdown._SHUTDOWN_TURN_S, platform.AGENT_WIND_DOWN_S - 30)

    def test_the_coordinators_last_turn_fits_inside_its_own_wind_down(self):
        # The coordinator writes the final summary in its wind-down.
        self.assertLessEqual(shutdown._COORDINATOR_SHUTDOWN_TURN_S, platform.COORDINATOR_WIND_DOWN_S - 60)
        self.assertGreater(shutdown._COORDINATOR_SHUTDOWN_TURN_S, shutdown._SHUTDOWN_TURN_S)


class ErrorTurnClient:
    """Ends its one turn with the error text the test set."""

    result = ""
    status = None
    options = None

    def __init__(self, options):
        ErrorTurnClient.options = options

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
                                client=FakePlatform(),
                                cfg=SimpleNamespace(agent_id="ag-1", role="executor", api_url="http://x", token=AGENT_TOKEN, platform_experiment_id="pe-1"))
        with mock.patch.object(connection, "ClaudeSDKClient", ErrorTurnClient), \
                mock.patch.object(ErrorTurnClient, "result", result), \
                mock.patch.object(ErrorTurnClient, "status", status), \
                contextlib.redirect_stdout(io.StringIO()):
            return await asyncio.wait_for(connect(setup, None, [False]), timeout=5)

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

    # The machine holds no model credential: the CLI reaches the model through the platform with
    # the agent's own token, and nothing else in its environment names a credential.
    async def test_the_cli_makes_its_model_calls_through_the_platform_with_the_agents_own_token(self):
        with mock.patch.dict(os.environ, {**LAUNCH_ENV, "CLAUDE_CONFIG_DIR": os.environ["CLAUDE_CONFIG_DIR"]}, clear=True):
            await self.session("API Error: 500 Internal server error", 500)
        env = ErrorTurnClient.options.env
        self.assertEqual(env["ANTHROPIC_BASE_URL"], "http://x/model/anthropic")
        self.assertEqual(env["CLAUDE_CODE_OAUTH_TOKEN"], AGENT_TOKEN)
        self.assertEqual(env["CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC"], "1")
        for key in ("ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "MODEL_ANTHROPIC_OAUTH_TOKEN"):
            self.assertNotIn(key, env)

    def test_each_provider_has_its_proxy_path_and_any_but_anthropic_names_its_model_for_every_call(self):
        cfg = SimpleNamespace(api_url="https://api.example/", token="t", role="executor")
        anthropic = cli.model_env(cfg, served(cfg, model="anthropic/claude-x"))
        self.assertEqual(anthropic["ANTHROPIC_BASE_URL"], "https://api.example/model/anthropic")
        self.assertFalse([key for key in anthropic if key.startswith("ANTHROPIC_DEFAULT_") or key == "CLAUDE_CODE_SUBAGENT_MODEL"])
        for provider, model_id in (("openrouter", "moonshotai/kimi-k2"), ("openai", "gpt-6-luna")):
            env = cli.model_env(cfg, served(cfg, model=f"{provider}/{model_id}"))
            self.assertEqual(env["ANTHROPIC_BASE_URL"], f"https://api.example/model/{provider}")
            self.assertEqual(env["CLAUDE_CODE_OAUTH_TOKEN"], "t")
            for key in ("ANTHROPIC_DEFAULT_OPUS_MODEL", "ANTHROPIC_DEFAULT_SONNET_MODEL", "ANTHROPIC_DEFAULT_HAIKU_MODEL",
                        "CLAUDE_CODE_SUBAGENT_MODEL"):
                self.assertEqual(env[key], model_id)

    def test_a_changed_provider_takes_a_new_session(self):
        a = served(SimpleNamespace(role="executor"), model="anthropic/x")
        self.assertTrue(a.needs_new_session(served(SimpleNamespace(role="executor"), model="openrouter/x")))
        self.assertFalse(a.needs_new_session(a))

    def test_a_model_reference_is_a_known_provider_and_a_model_id(self):
        self.assertEqual(instructions.split_model("openrouter/moonshotai/kimi-k2"), ("openrouter", "moonshotai/kimi-k2"))
        for reference in ("claude-x", "", "anthropic/", "/claude-x", "google/gemini", "Anthropic/claude-x"):
            with self.assertRaises(SystemExit):
                instructions.split_model(reference)

    def test_a_context_that_overflowed_is_no_refused_request(self):
        overflow = ResultMessage(subtype="success", duration_ms=1, duration_api_ms=1, is_error=True, num_turns=1,
                                 session_id="s-1", result="Prompt is too long", api_error_status=400)
        self.assertFalse(results.request_refused(overflow))

    def test_only_the_providers_refusal_of_the_credential_counts(self):
        self.assertFalse(results.credential_refused(None))
        self.assertFalse(results.credential_refused("rate_limit_error: too many requests"))
        self.assertTrue(results.credential_refused("NOT LOGGED IN"))

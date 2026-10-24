import asyncio
import unittest
from types import SimpleNamespace
from unittest import mock

from hypothesisloop_agent import api_client, core, outage

REFUSED = api_client.APIError("GET", "http://api/x", 401, '{"message":"unauthenticated: agent ag-1 finished in platform experiment pe-1"}')
DOWN = api_client.APIError("GET", "http://api/x", 0, "connection refused")
GATEWAY = api_client.APIError("GET", "http://api/x", 502, "<html>502 Bad Gateway</html>")
BROKEN = api_client.APIError("GET", "http://api/x", 500, '{"reason":"internal","message":"internal error"}')


def setup(answer):
    """An agent whose signup read answers with answer(), a dict or an error to raise."""
    async def get_signup(*_):
        result = answer()
        if isinstance(result, Exception):
            raise result
        return result

    cfg = SimpleNamespace(agent_id="ag-1", role="executor", platform_experiment_id="pe-1")
    return SimpleNamespace(cfg=cfg, client=SimpleNamespace(get_signup=get_signup))


# A stop check that could not ask the platform logged "continuing" and went on: with its token
# refused after the wind-down, or the platform gone, a machine nothing outside could stop ran on.
def no_outage():
    outage._since, outage._over, outage._suspected = None, None, False


class RunOverTest(unittest.IsolatedAsyncioTestCase):
    def setUp(self):
        no_outage()
        self.addCleanup(no_outage)

    async def test_a_refused_token_is_a_run_that_is_over(self):
        reason = await core.run_over(setup(lambda: REFUSED))
        self.assertEqual(reason, 'the platform refuses this token, so its run is over ({"message":"unauthenticated: agent ag-1 finished in platform experiment pe-1"})')
        self.assertTrue(core.lost_platform(reason))
        self.assertEqual(await core.stop_reason(setup(lambda: REFUSED), [False]), reason)

    async def test_an_outage_is_waited_out_up_to_the_limit_and_an_answer_ends_it(self):
        answers = [DOWN, GATEWAY, {}, DOWN]
        agent = setup(lambda: answers.pop(0))
        with self.assertLogs("llm_agent", "WARNING") as logged:
            self.assertIsNone(await core.run_over(agent))
            self.assertIsNone(await core.run_over(agent))
        self.assertEqual(len(logged.records), 1)
        self.assertTrue(outage.under_way())
        self.assertEqual(outage.over(), [])
        self.assertIsNone(await core.run_over(agent))
        self.assertFalse(outage.under_way())
        self.assertEqual([e["kind"] for e in outage.over()], ["platform.reachable"])
        self.assertEqual(outage.over(), [])
        with self.assertLogs("llm_agent", "WARNING"):
            self.assertIsNone(await core.run_over(agent))

    # A refusal the platform itself answered is an answer: asked again at the next read, it
    # starts no outage and ends one under way.
    async def test_a_server_error_in_json_starts_no_outage_and_ends_one(self):
        with self.assertLogs("llm_agent", "WARNING"):
            self.assertIsNone(await core.run_over(setup(lambda: BROKEN)))
        self.assertFalse(outage.under_way())
        self.assertEqual(outage.over(), [])
        answers = [DOWN, BROKEN]
        agent = setup(lambda: answers.pop(0))
        with self.assertLogs("llm_agent", "WARNING"):
            await core.run_over(agent)
            self.assertTrue(outage.under_way())
            self.assertIsNone(await core.run_over(agent))
        self.assertFalse(outage.under_way())
        self.assertEqual([e["kind"] for e in outage.over()], ["platform.reachable"])

    async def test_a_run_the_platform_ended_is_no_lost_platform(self):
        reason = await core.run_over(setup(lambda: {"ended": "cut"}))
        self.assertEqual(reason, "the platform ended the run: cut")
        self.assertFalse(core.lost_platform(reason))
        self.assertFalse(core.lost_platform(None))


class RunUntilLostTest(unittest.IsolatedAsyncioTestCase):
    def setUp(self):
        no_outage()

    async def test_a_lost_platform_ends_the_process_whatever_is_in_flight(self):
        dropped = []

        async def turn_in_flight():
            try:
                await asyncio.sleep(60)
            finally:
                dropped.append(True)

        answers = [{}, REFUSED]
        with mock.patch.object(core, "_RUN_CHECK_INTERVAL_S", 0.01), self.assertRaises(SystemExit) as exited:
            await asyncio.wait_for(core.run_until_lost(setup(lambda: answers.pop(0)), turn_in_flight(), [False]), timeout=5)
        self.assertTrue(str(exited.exception).startswith("agent: exiting: the platform refuses this token, so its run is over"))
        self.assertEqual(dropped, [True])

    async def test_work_that_ends_or_fails_is_what_the_process_ends_with(self):
        async def done():
            return None

        async def broken():
            raise KeyError("session_id")

        await asyncio.wait_for(core.run_until_lost(setup(dict), done(), [False]), timeout=5)
        with self.assertRaises(KeyError):
            await asyncio.wait_for(core.run_until_lost(setup(dict), broken(), [False]), timeout=5)

    # An experiment reached its end while its coordinator was in a turn. No signal had come when
    # the turn ended, so the process exited with no last turn and the summary was never written.
    async def test_a_run_the_platform_ended_asks_for_the_stop_and_leaves_the_work_its_last_turn(self):
        stop_flag = [False]

        async def last_turn():
            while not stop_flag[0]:
                await asyncio.sleep(0.01)
            await asyncio.sleep(0.1)
            return "summary filed"

        with mock.patch.object(core, "_RUN_CHECK_INTERVAL_S", 0.01), self.assertLogs("llm_agent"):
            filed = await asyncio.wait_for(core.run_until_lost(setup(lambda: {"ended": "cut"}), last_turn(), stop_flag), timeout=5)
        self.assertEqual(filed, "summary filed")


if __name__ == "__main__":
    unittest.main()

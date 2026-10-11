import unittest
from unittest import mock

from hypothesisloop_agent.claude import pacing

HOUR = 3600.0


class AllowanceTest(unittest.TestCase):
    def setUp(self):
        self.now = 1_000_000.0
        self.allowance = pacing.Allowance(clock=lambda: self.now)

    # Three agents on one account used its five-hour window in three hours: past an hour in,
    # a third of the window was used with a fifth of it gone.
    def test_a_window_used_further_than_its_time_is_paced(self):
        self.allowance.read("five_hour", 0.35, self.now + 4 * HOUR)
        self.assertEqual(self.allowance.pacing(), pacing.Reading("five_hour", 0.35, self.now + 4 * HOUR))
        self.assertEqual(self.allowance.cap()[0], pacing.SUBAGENTS)
        self.assertIn("used 35% of its five_hour window with 20% of its time gone", self.allowance.note(coordinator=False))
        self.assertNotIn("regular review", self.allowance.note(coordinator=False))
        self.assertIn("regular review", self.allowance.note(coordinator=True))

    def test_a_window_on_pace_early_or_reset_is_not(self):
        self.allowance.read("five_hour", 0.45, self.now + 2 * HOUR)
        self.allowance.read("seven_day", 0.25, self.now + 6 * 24 * HOUR)
        self.assertIsNone(self.allowance.pacing(), "five_hour on pace, seven_day under the floor")
        self.assertEqual(self.allowance.note(coordinator=True), "")
        self.assertIsNone(self.allowance.cap())
        self.allowance.read("five_hour", 0.95, self.now - 1)
        self.assertIsNone(self.allowance.pacing(), "a window that reset")
        self.allowance.read("unknown_window", 0.99, self.now + HOUR)
        self.allowance.read("five_hour", None, self.now + HOUR)
        self.assertIsNone(self.allowance.pacing())

    def test_the_window_furthest_ahead_is_said(self):
        self.allowance.read("five_hour", 0.5, self.now + 4 * HOUR)
        self.allowance.read("seven_day", 0.9, self.now + 5 * 24 * HOUR)
        self.assertEqual(self.allowance.pacing().window, "seven_day")

    # Measured on the bundled CLI: get_usage answers each window's utilization in percent and its
    # reset in ISO 8601, a window the account does not have as null.
    def test_the_clis_usage_is_read_in_percent(self):
        self.allowance.read_usage({"rate_limits": {
            "five_hour": {"utilization": 70, "resets_at": "1970-01-12T15:46:40+00:00"},
            "seven_day": {"utilization": 94, "resets_at": "not a time"},
            "seven_day_opus": None, "extra_usage": {"is_enabled": False, "utilization": None}, "limits": []}})
        self.assertEqual(self.allowance.furthest(), pacing.Reading("five_hour", 0.7, self.now + 2 * HOUR))
        self.allowance.read_usage({"rate_limits": None})

    # Run 15 had a week's window 53% used with 15% of it gone and seven hours left of the run:
    # held to two subagents, eight job places stood empty.
    def test_a_run_ending_before_the_reset_is_paced_to_its_end(self):
        self.allowance.read("seven_day", 0.53, self.now + (7 * 24 - 25) * HOUR)
        self.assertIsNotNone(self.allowance.pacing(), "paced to the reset, 15% gone")
        self.allowance.until(self.now + 7 * HOUR)
        self.assertAlmostEqual(self.allowance.furthest().gone(self.now, self.now + 7 * HOUR), 25 / 32)
        self.assertIsNone(self.allowance.pacing(), "78% of the time to the run's end gone")
        self.allowance.until(self.now + 30 * 24 * HOUR)
        self.assertIsNotNone(self.allowance.pacing(), "a run past the reset is paced to the reset")
        self.allowance.until(None)
        self.assertIsNotNone(self.allowance.pacing())

    def test_the_thresholds_are_set_from_the_environment(self):
        with mock.patch.object(pacing, "MARGIN", 0.3):
            self.allowance.read("five_hour", 0.6, self.now + 3 * HOUR)
            self.assertIsNone(self.allowance.pacing())


if __name__ == "__main__":
    unittest.main()

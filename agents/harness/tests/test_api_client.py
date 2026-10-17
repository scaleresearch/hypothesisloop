import io
import unittest
import urllib.error
from unittest import mock

from hypothesisloop_agent import api_client


class Answer(io.BytesIO):
    def __enter__(self):
        return self

    def __exit__(self, *args):
        return False


def http_error(status: int) -> urllib.error.HTTPError:
    return urllib.error.HTTPError("http://api/x", status, "", {}, io.BytesIO(b"from the gateway"))


class SendTest(unittest.TestCase):
    def send(self, method, *answers):
        with mock.patch.object(api_client.urllib.request, "urlopen", side_effect=list(answers)) as urlopen, \
                mock.patch.object(api_client.time, "sleep") as sleep:
            try:
                return api_client._send(method, "http://api/x", "token"), urlopen.call_count, sleep.call_args_list
            except api_client.APIError as e:
                return e, urlopen.call_count, sleep.call_args_list

    # The hosted coordinator died of a read that timed out through the public tunnel.
    def test_a_get_that_times_out_or_meets_a_gateway_is_asked_again_with_backoff(self):
        body, calls, sleeps = self.send("GET", TimeoutError("The read operation timed out"),
                                        ConnectionResetError("reset"), http_error(502), http_error(504), Answer(b"{}"))
        self.assertEqual((body, calls), (b"{}", 5))
        self.assertEqual([c.args[0] for c in sleeps], [1.0, 2.0, 4.0, 8.0])

    def test_a_get_that_keeps_failing_says_why_and_how_often_it_was_asked(self):
        error, calls, _ = self.send("GET", *[http_error(503) for _ in range(5)])
        self.assertEqual((error.status, calls), (503, 5))
        self.assertIn("from the gateway", str(error))
        self.assertIn("gave up after 5 attempts", str(error))
        error, _, _ = self.send("GET", *[TimeoutError("The read operation timed out")] * 5)
        self.assertEqual(error.status, 0)
        self.assertIn("TimeoutError: The read operation timed out", str(error))

    def test_a_refusal_or_a_server_error_is_not_asked_again(self):
        for status in (401, 404, 500):
            error, calls, _ = self.send("GET", http_error(status), Answer(b"{}"))
            self.assertEqual((error.status, calls), (status, 1))
            self.assertNotIn("gave up", str(error))

    def test_a_write_is_never_asked_again(self):
        for method in ("POST", "PUT", "PATCH", "DELETE"):
            for failure in (TimeoutError("timed out"), http_error(503)):
                error, calls, sleeps = self.send(method, failure, Answer(b"{}"))
                self.assertIsInstance(error, api_client.APIError)
                self.assertEqual((calls, sleeps), (1, []))


if __name__ == "__main__":
    unittest.main()

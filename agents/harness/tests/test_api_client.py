import unittest

import httpx

from hypothesisloop_agent import api_client, platform


def client_answering(*answers) -> tuple[api_client.PlatformClient, list[httpx.Request]]:
    """A PlatformClient whose requests get answers in turn: an httpx.Response, or an exception raised."""
    sent, left = [], list(answers)

    def handle(request: httpx.Request) -> httpx.Response:
        sent.append(request)
        answer = left.pop(0)
        if isinstance(answer, Exception):
            raise answer
        return answer

    client = api_client.PlatformClient("http://api", "token")
    client._http = httpx.AsyncClient(base_url="http://api", transport=httpx.MockTransport(handle))
    return client, sent


class SendTest(unittest.IsolatedAsyncioTestCase):
    async def test_every_call_is_made_once_and_its_failure_reaches_the_caller_as_it_came(self):
        for method in ("GET", "PUT"):
            for failure, status in ((httpx.ReadTimeout("The read operation timed out"), 0),
                                    (httpx.Response(503, text="from the gateway"), 503),
                                    (httpx.Response(500, json={"reason": "internal"}), 500)):
                client, sent = client_answering(failure, httpx.Response(200, content=b"{}"))
                with self.assertRaises(api_client.APIError) as raised:
                    await client._send(method, "/x", content=b"" if method == "PUT" else None)
                self.assertEqual((raised.exception.status, len(sent)), (status, 1), (method, failure))
        client, _ = client_answering(httpx.ReadTimeout("The read operation timed out"))
        with self.assertRaises(api_client.APIError) as raised:
            await client._send("GET", "/x")
        self.assertIn("ReadTimeout: The read operation timed out", str(raised.exception))

    async def test_an_answer_is_its_body(self):
        client, _ = client_answering(httpx.Response(200, json={"status": "running"}))
        self.assertEqual(await client.get_json("/x"), {"status": "running"})


class UnreachableTest(unittest.TestCase):
    def test_no_answer_or_a_gateway_page_is_unreachable_and_an_answer_of_the_platform_is_not(self):
        def error(status, body):
            return api_client.APIError("GET", "/x", status, body)

        self.assertTrue(error(0, "ConnectError: connection refused").unreachable)
        for status in sorted(platform.GATEWAY_STATUSES):
            self.assertTrue(error(status, "<html><body>502 Bad Gateway</body></html>").unreachable, status)
            self.assertFalse(error(status, '{"reason": "unavailable", "message": "draining"}').unreachable, status)
        self.assertFalse(error(500, "<html>internal</html>").unreachable)
        self.assertFalse(error(500, '{"reason": "internal"}').unreachable)
        self.assertFalse(error(401, "unauthenticated").unreachable)


if __name__ == "__main__":
    unittest.main()

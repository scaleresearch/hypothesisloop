import unittest
from datetime import datetime, timezone

from claude_agent_sdk import (
    AssistantMessage,
    RateLimitEvent,
    RateLimitInfo,
    SystemMessage,
    TextBlock,
    ToolResultBlock,
    ToolUseBlock,
    UserMessage,
)

from hypothesisloop_agent.claude import activity

NOW = datetime(2026, 10, 2, 6, 21, 30, tzinfo=timezone.utc)


def assistant(*blocks):
    return AssistantMessage(content=list(blocks), model="m")


def failed(content):
    return UserMessage(content=[ToolResultBlock(tool_use_id="t", content=content, is_error=True)])


class ActivityLinesTest(unittest.TestCase):
    def test_text_is_one_truncated_line(self):
        [line] = activity.lines(assistant(TextBlock(text="first\n\nsecond " + "x" * 500)), "agent-a", NOW)
        self.assertTrue(line.startswith("2026-10-02T06:21:30Z agent-a say   first ⏎ second x"))
        self.assertTrue(line.endswith("..."))
        self.assertEqual(len(line.split(" say   ", 1)[1]), activity.WIDTH)

    def test_blank_text_is_skipped(self):
        self.assertEqual(activity.lines(assistant(TextBlock(text="  \n")), "a", NOW), [])

    def test_bash_shows_the_command(self):
        block = ToolUseBlock(id="t", name="Bash", input={
            "command": "hl hypothesis add 'wider MLP'\n  --builds-on H1", "description": "add"})
        self.assertEqual(activity.lines(assistant(block), "a", NOW),
                         ["2026-10-02T06:21:30Z a tool  Bash: hl hypothesis add 'wider MLP' ⏎ --builds-on H1"])

    def test_other_tools_show_their_main_input(self):
        read = ToolUseBlock(id="t", name="Read", input={"file_path": "/w/train.py", "limit": 5})
        odd = ToolUseBlock(id="u", name="Odd", input={"n": 1})
        self.assertEqual(activity.lines(assistant(read, odd), "a", NOW), [
            "2026-10-02T06:21:30Z a tool  Read: /w/train.py",
            '2026-10-02T06:21:30Z a tool  Odd: {"n": 1}',
        ])

    def test_failed_result_shows_exit_code_and_first_error_line(self):
        message = failed("Exit code 1\nhl: 409 similar hypothesis exists: H7\nmore detail")
        self.assertEqual(activity.lines(message, "a", NOW),
                         ["2026-10-02T06:21:30Z a error exit 1: hl: 409 similar hypothesis exists: H7"])

    def test_failed_result_strips_tool_error_tags_and_reads_text_parts(self):
        self.assertEqual(activity.lines(failed("<tool_use_error>File not read</tool_use_error>"), "a", NOW),
                         ["2026-10-02T06:21:30Z a error File not read"])
        self.assertEqual(activity.lines(failed([{"type": "text", "text": "boom\nx"}]), "a", NOW),
                         ["2026-10-02T06:21:30Z a error boom"])

    def test_failed_traceback_shows_the_error_line(self):
        message = failed('Exit code 1\nTraceback (most recent call last):\n  File "t.py", line 3, in <module>\n'
                         '    run()\nValueError: lr must be positive\n\n')
        self.assertEqual(activity.lines(message, "a", NOW),
                         ["2026-10-02T06:21:30Z a error exit 1: ValueError: lr must be positive"])

    def test_successful_result_is_not_logged(self):
        message = UserMessage(content=[ToolResultBlock(tool_use_id="t", content="ok", is_error=False)])
        self.assertEqual(activity.lines(message, "a", NOW), [])

    # A model call took 397 s for 669 tokens and nothing in the agent's log said a call was out.
    def test_a_model_call_that_answered_late_is_said_once_with_its_tokens(self):
        message = AssistantMessage(content=[TextBlock(text="done")], model="m", usage={"output_tokens": 669})
        self.assertEqual(activity.lines(message, "a", NOW, waited=397.2), [
            "2026-10-02T06:21:30Z a slow  model call answered after 397 s with 669 output tokens",
            "2026-10-02T06:21:30Z a say   done"])
        self.assertEqual(len(activity.lines(message, "a", NOW, waited=activity.SLOW_CALL_S - 1)), 1)

    def test_a_retried_model_call_and_a_rate_limit_are_each_one_line(self):
        retry = SystemMessage(subtype="api_retry", data={"attempt": 2, "max_retries": 10, "retry_delay_ms": 4200,
                                                         "error_status": 529, "error": "overloaded"})
        self.assertEqual(activity.lines(retry, "a", NOW),
                         ["2026-10-02T06:21:30Z a retry model call failed (529 overloaded): attempt 2 of 10 in 4 s"])
        limited = RateLimitEvent(RateLimitInfo(status="rejected", resets_at=1791094890, rate_limit_type="five_hour"), "u", "s")
        self.assertEqual(activity.lines(limited, "a", NOW),
                         ["2026-10-02T06:21:30Z a limit rate limit rejected (five_hour), resets 2026-10-04T06:21:30Z"])
        self.assertEqual(activity.lines(RateLimitEvent(RateLimitInfo(status="allowed"), "u", "s"), "a", NOW), [])
        self.assertEqual(activity.lines(SystemMessage(subtype="init", data={}), "a", NOW), [])


def bash(command):
    return assistant(ToolUseBlock(id="t", name="Bash", input={"command": command}))


# What the platform shows of an agent beside its machine is read by its whole organisation. It
# names the command, the file or the id of the tool call in flight, and nothing an argument holds.
class DoingTest(unittest.TestCase):
    def test_an_hl_command_is_said_by_its_words_and_its_ids(self):
        job = "job-0199a1b2c3d4e5f60718293a4b5c6d7e"
        for command, said in {
            "hl capacity": "running hl capacity",
            "hl platform-experiments agents add --hosted --role baseline": "running hl platform-experiments agents add",
            f"hl job status {job} --json": f"running hl job status {job}",
            f"cd /w/repo && API_URL=http://x hl job logs {job} | tail -5": f"running hl job logs {job}",
            "hl job submit job.yaml": "running hl job submit",
            "/usr/local/bin/hl quota": "running hl quota",
        }.items():
            self.assertEqual(activity.doing(bash(command)), said, command)

    def test_no_argument_of_a_command_is_said(self):
        secret = "hls_c2VjcmV0.c2lnbmF0dXJl"
        for command, said in {
            f"curl -H 'Authorization: Bearer {secret}' https://api.example.com/x": "running curl",
            f"HL_TOKEN={secret} hl hypothesis comment --text 'the key is {secret}'": "running hl hypothesis comment",
            f"hl login {secret}": "running hl login",
            f"export TOKEN={secret}; python train.py --key {secret}": "running python train.py",
            f"git clone https://user:{secret}@github.com/acme/model.git": "running git clone",
            f"echo {secret} > .env": "running echo",
            f"{secret}": "running a command",
            "": "running a command",
            "echo 'an unclosed quote": "running echo",
        }.items():
            got = activity.doing(bash(command))
            self.assertEqual(got, said, command)
            self.assertNotIn(secret, got)

    def test_a_file_tool_is_said_by_the_files_name_and_no_content(self):
        read = ToolUseBlock(id="t", name="Read", input={"file_path": "/w/repo/train.py"})
        write = ToolUseBlock(id="t", name="Write", input={"file_path": "/w/proposal.yaml", "content": "password: hunter2"})
        odd = ToolUseBlock(id="t", name="Edit", input={"file_path": "/w/a file with spaces", "old_string": "a", "new_string": "b"})
        self.assertEqual(activity.doing(assistant(read)), "reading train.py")
        self.assertEqual(activity.doing(assistant(write)), "writing proposal.yaml")
        self.assertEqual(activity.doing(assistant(odd)), "editing a file")
        self.assertEqual(activity.doing(assistant(ToolUseBlock(id="t", name="Grep", input={"pattern": "hunter2"}))), "searching the repository")
        self.assertEqual(activity.doing(assistant(ToolUseBlock(id="t", name="Odd", input={"n": "hunter2"}))), "using Odd")

    def test_the_last_call_of_a_message_stands_and_an_answer_is_thinking_again(self):
        both = assistant(TextBlock(text="first the capacity"), ToolUseBlock(id="t", name="Read", input={"file_path": "a.py"}),
                         ToolUseBlock(id="u", name="Bash", input={"command": "hl capacity"}))
        self.assertEqual(activity.doing(both), "running hl capacity")
        answered = UserMessage(content=[ToolResultBlock(tool_use_id="t", content="ok", is_error=False)])
        self.assertEqual(activity.doing(answered), "thinking")
        # What the agent says changes nothing, nor does anything a subagent does.
        self.assertEqual(activity.doing(assistant(TextBlock(text="done"))), "")
        inside = AssistantMessage(content=[ToolUseBlock(id="t", name="Bash", input={"command": "ls"})], model="m", parent_tool_use_id="task-1")
        self.assertEqual(activity.doing(inside), "")
        self.assertEqual(activity.doing(SystemMessage(subtype="init", data={})), "")


if __name__ == "__main__":
    unittest.main()

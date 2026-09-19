import unittest
from datetime import datetime, timezone

from claude_agent_sdk import AssistantMessage, TextBlock, ToolResultBlock, ToolUseBlock, UserMessage

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


if __name__ == "__main__":
    unittest.main()

import unittest
from datetime import datetime, timezone

from claude_agent_sdk import (
    AssistantMessage,
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
    def test_failed_result_shows_exit_code_and_first_error_line(self):
        message = failed("Exit code 1\nhl: 409 similar hypothesis exists: H7\nmore detail")
        self.assertEqual(activity.lines(message, "a", NOW),
                         ["2026-10-02T06:21:30Z a error exit 1: hl: 409 similar hypothesis exists: H7"])

    def test_failed_traceback_shows_the_error_line(self):
        message = failed('Exit code 1\nTraceback (most recent call last):\n  File "t.py", line 3, in <module>\n'
                         '    run()\nValueError: lr must be positive\n\n')
        self.assertEqual(activity.lines(message, "a", NOW),
                         ["2026-10-02T06:21:30Z a error exit 1: ValueError: lr must be positive"])


def bash(command):
    return assistant(ToolUseBlock(id="t", name="Bash", input={"command": command}))


# What the platform shows of an agent beside its machine is read by its whole organisation. It
# names the command, the file or the id of the tool call in flight, and nothing an argument holds.
class DoingTest(unittest.TestCase):
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

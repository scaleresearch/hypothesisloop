"""How the model CLI is started: its tools, its environment and the options of a connection."""
from __future__ import annotations

from claude_agent_sdk import ClaudeAgentOptions, HookMatcher

from hypothesisloop_agent import config, core, instructions

# These are interactive-harness features (a resumable Claude Code session with a human/UI on the
# other end) that don't do anything meaningful inside this worker, but the SDK's default tool set
# still advertises them — and the model, seeing them, reasonably tries to use them to "wait" for a
# background job. Confirmed live: an agent called ScheduleWakeup expecting to be resumed later and
# just ended its turn instead, because nothing here ever fires that wakeup. Strip them so the model
# never reaches for a tool that can't do what its name promises in this environment. TaskStop
# stays, for the agent to stop a subagent that went astray (hooks.py).
DISALLOWED_TOOLS = ["ScheduleWakeup", "Monitor", "TaskCreate", "TaskUpdate", "TaskGet", "TaskList",
                     "TaskOutput", "CronCreate", "CronDelete", "CronList", "PushNotification",
                     "RemoteTrigger", "DesignSync",
                     # A subagent's worktree is `git worktree add`, as the briefing says: agents
                     # called EnterWorktree instead and it failed.
                     "EnterWorktree", "ExitWorktree"]

# The CLI compacts a session near the model's window, 1M or so, and subagents, which the platform
# never rotates, re-read 400-450K on every call. Past this window less about 33K it compacts each
# session, a subagent's too (CLAUDE_CODE_AUTO_COMPACT_WINDOW, 100K to 1M).
AUTO_COMPACT_WINDOW_TOKENS = 200_000

# The CLI defers WebSearch and WebFetch behind ToolSearch by default: a planner had to call
# ToolSearch "select:WebSearch" first, and in live runs most researchers and planners never
# searched at all. With tool search off every allowed tool is sent whole, so these two are directly callable.
CLI_ENV = {"ENABLE_TOOL_SEARCH": "false",
           # Background tasks stay on so subagents can run in the background: the CLI starts the
           # lead's next turn when one ends. A backgrounded command is refused (hooks.py), since
           # agents polled their output files; the 10 min timeout keeps long commands in the
           # foreground. The briefings say every command starts in the working directory: the CLI
           # keeps a `cd` otherwise, and an agent that repeated one failed.
           "BASH_DEFAULT_TIMEOUT_MS": "600000", "CLAUDE_BASH_MAINTAIN_PROJECT_WORKING_DIR": "1",
           # The CLI caches a subagent's context for 5 min, and the lead's too on an API key or in
           # overage: a subagent whose `hl job wait` returns every 9 min would write its whole
           # context again on each wake. Every call is cached for an hour.
           "ENABLE_PROMPT_CACHING_1H": "1",
           "CLAUDE_CODE_AUTO_COMPACT_WINDOW": str(AUTO_COMPACT_WINDOW_TOKENS)}


def model_env(cfg: config.Config) -> dict[str, str]:
    """Where the CLI makes its model calls: the platform, which makes each with the platform's own
    credential (controlplane/httpapi/modelproxy), reached with the agent's own token. The token
    goes in as the CLI's OAuth token, so the CLI sends what it sends with a Claude subscription
    and reads the account's rate limits from every answer, which it ignored with the token as
    ANTHROPIC_AUTH_TOKEN. With its nonessential traffic off it sends the token nowhere else."""
    return {"ANTHROPIC_BASE_URL": cfg.api_url.rstrip("/") + "/model/anthropic",
            "CLAUDE_CODE_OAUTH_TOKEN": cfg.token,
            "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1"}


# The CLI writes each message as one line, and the SDK ends the session on a line past its bound,
# 1 MB by default: a researcher whose six subagents read papers lost its session to one.
# A request to the model is at most 32 MB, so no message of a session is longer.
MAX_MESSAGE_BYTES = 32 * 1024 * 1024


# The CLI's Write refuses an existing file the session has not opened with its Read, and no
# setting lifts that: an agent that had only `cat` a file was refused its Write.
FILE_TOOLS_NOTE = "\n\nWrite and Edit refuse an existing file you have not opened with Read: `cat` does not count."


def options(setup: core.RunSetup, running: instructions.Instructions, resume_id: str | None,
            hooks: dict[str, list[HookMatcher]]) -> ClaudeAgentOptions:
    """The options of a connection that runs running, resuming resume_id when it is set."""
    return ClaudeAgentOptions(
        system_prompt=running.prompt + FILE_TOOLS_NOTE,
        model=running.model,
        effort=running.effort,
        # The stream events of a model call say what it writes while it writes, see activity.Composing.
        include_partial_messages=True,
        max_buffer_size=MAX_MESSAGE_BYTES,
        permission_mode="bypassPermissions",
        # No max_turns cap: this session is meant to run for the platform experiment's whole
        # lifetime (hours to days), not a bounded number of turns. The only real stop conditions
        # are a signal and the end of its run on the platform, see core.stop_reason.
        max_turns=None,
        cwd=setup.workdir,
        resume=resume_id,
        # This process must only ever expose the standard Claude Code tool set — nothing pulled in
        # from whatever machine happens to run it, so a run is reproducible regardless of host.
        # Without these two, the bundled CLI still loads
        # user-level ~/.claude/settings.json and any configured MCP servers, which on a dev
        # machine that also runs interactive Claude Code sessions silently hands the agent extra
        # tools (Monitor, ScheduleWakeup, TaskCreate, ...) that only work inside a resumable
        # interactive session — a detached one-shot subprocess calling them just hangs forever.
        setting_sources=[],
        strict_mcp_config=True,
        disallowed_tools=DISALLOWED_TOOLS,
        env={**CLI_ENV, **model_env(setup.cfg), **core.git_identity(setup.cfg)},
        # The CLI says each prompt again as the turn that takes it up starts, see turns.answer.
        extra_args={"replay-user-messages": None},
        hooks=hooks,
    )

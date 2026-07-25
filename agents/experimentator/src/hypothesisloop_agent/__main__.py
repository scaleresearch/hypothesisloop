"""Back-compat entry point: `python -m hypothesisloop_agent` runs the Claude backend, as before
the claude/codex split. Pick a backend explicitly with `python -m hypothesisloop_agent.claude.run`
or `python -m hypothesisloop_agent.codex.run` (or the hypothesisloop-agent-{claude,codex} scripts).
"""
from hypothesisloop_agent.claude.run import main

if __name__ == "__main__":
    main()

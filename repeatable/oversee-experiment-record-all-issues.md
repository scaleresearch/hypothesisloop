# Oversee experiment & record all issues — master prompt

Launch and run one full experiment end to end, fully unattended. Fill in the variables, then
follow the body below.

## Variables

    EXPERIMENT=sparse-sdpa            # dir name under agents/coordinator/experiments/<name>/
    NUMBER_OF_STAGES=3                # length of the elimination ladder
    STAGE1_TARGET_JOB_MINUTES=15      # stage 1 caps jobs this long, to force broad exploration first
    TOTAL_RUN_HOURS=7                 # ~6-8h total; used to size budget_accelerator_hours
    NUM_AGENTS=2
    CHIPS_PER_AGENT=2
    FINDINGS_FILE=fix-later.md        # long-term findings — repo root, created if absent

## Task

Run a `$NUMBER_OF_STAGES`-stage experiment on `$EXPERIMENT`. Stage 1 caps jobs at around
`$STAGE1_TARGET_JOB_MINUTES` min to force broad exploration before longer runs; total ~
`$TOTAL_RUN_HOURS`h. First run of this setup — also treat it as a dry run of the process: note
what works/doesn't as you go.

Act as the coordinator: `agents/coordinator/setup.md` (capacity, platform-experiment creation,
spawning) then `agents/coordinator/supervise.md` (watch loop, findings, wind-down) — read both,
they're the operating manual; this file is only the run-specific assignment on top.

- Sign up `$NUM_AGENTS` agents, `$CHIPS_PER_AGENT` chips each, after creating the platform
  experiment (needs a real id).
- Push agents to actually pursue the experiment's criteria, not coast on the first working config.
- Fix blockers as they come up; record anything that'd speed up the next run in `$FINDINGS_FILE`
  — a fact plus why it matters, not a narrative.
- If the platform experiment itself breaks irrecoverably, close and restart clean — log why first.
- Act fully independently: no confirmation, no clarification, ever. Decide, record, continue.
- Be concise everywhere — reasoning and `$FINDINGS_FILE` entries alike.

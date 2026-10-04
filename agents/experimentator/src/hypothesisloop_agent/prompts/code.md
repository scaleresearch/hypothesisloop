Where an agent's code lives, the platform experiment's repository, spliced into the briefings by
core.system_prompt: a section per placeholder, each opened by a line of ==== and its name, and
{start} the start_ section of the agent's role.

==== workspace
run `git clone {repository_url} .` in {workdir}, the directory every command starts
    in. {start}
==== start_baseline
Work on branch `hl/{platform_experiment_id}/baseline`; `hl job submit` pushes it. A run that
    `continues` another starts that branch from the other's `baseline_code_ref` and measures
    the control with its harness, changed only where it breaks the brief's definition of a
    metric (say so in `README.md`), so the numbers of both compare. Run
    `hl repo init` in the clone before you write anything: it writes the experiment's files
    from what the platform holds. `hl repo check` says what is left.
==== start_researcher
`hl hypothesis checkout ID` puts the clone on your branch of hypothesis ID, made
    from the code it builds on (the baseline's for a root child), after a restart too. Its
    harness, environment, `metrics.py` and `.hypothesisloop/job.yaml` stay as they are unless
    the idea is about them. Commit there; `hl job submit` pushes it.
==== repository_line
  repository: {repository_url}
==== start_code
  3. To read what the agents start from, run `git clone {repository_url} .` in {workdir}.
     Each agent pushes a branch per hypothesis under `hl/{platform_experiment_id}/`. Your access
     is read-only, and nothing you write locally outlives your machine.

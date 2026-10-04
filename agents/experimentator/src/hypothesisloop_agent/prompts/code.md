Where an agent's code lives, the platform experiment's repository, spliced into the briefings by
core.system_prompt: a section per placeholder, each opened by a line of ==== and its name, and
{start} the start_ section of the agent's role.

==== workspace
run `git clone {repository_url} .` in {workdir}, the directory you start
     in and every command starts in again. {start}
==== start_baseline
Work on branch
     `hl/{platform_experiment_id}/baseline` (`git checkout -b` it once): `hl job submit`
     pushes it. Run `hl repo init` in the clone before you write anything: it writes the
     experiment's files from what the platform holds and says which it kept. Its job.yaml is
     the one you complete and submit, once `hl repo check` passes.
==== start_researcher
`hl hypothesis checkout ID`,
     after a restart too, puts the clone on your branch of hypothesis ID, made from the code it
     builds on: the baseline's for a root child. Its harness, environment, `metrics.py` and
     `.hypothesisloop/job.yaml` (the base job spec) stay as they are unless the idea is about
     them. Commit there: `hl job submit` pushes it. A different claim, or a second way to
     improve your job, is a child hypothesis; another implementation of a claim, yours or
     another agent's, is your branch of its hypothesis. To drop an idea, settle it and register
     a new hypothesis on `root`.
==== repository_line
  repository: {repository_url}
==== start_code
  3. To read what the agents start from, run `git clone {repository_url} .` in
     {workdir}, the directory you start in: when it has them,
     `.hypothesisloop/experiment.yaml` is the base of the brief and `.hypothesisloop/job.yaml` the
     base job spec. Each agent pushes a branch per hypothesis under `hl/{platform_experiment_id}/`. Your access to
     the repository is read-only, and nothing you write locally outlives your machine.

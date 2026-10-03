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
A branch is your
     implementation of one hypothesis, named by hl: before working on an idea, and after a
     restart, `hl hypothesis checkout ID` puts the clone on yours. Commit there:
     `hl job submit` pushes it. A different claim, or a second way to improve
     your job, is a child hypothesis; another
     implementation of a claim, yours or another agent's, is your branch of its hypothesis.
     To drop an idea, settle it and register a new hypothesis on `root`.
     `.hypothesisloop/job.yaml`, if present, is the base job spec.
     Jobs report metrics with `post_metric` of the root `metrics.py`.
==== repository_line
  repository: {repository_url}
==== start_code
  3. To read what the agents start from, run `git clone {repository_url} .` in
     {workdir}, the directory you start in: when it has them,
     `.hypothesisloop/experiment.yaml` is the base of the brief and `.hypothesisloop/job.yaml` the
     base job spec. Each agent pushes a branch per hypothesis under `hl/{platform_experiment_id}/`. Your access to
     the repository is read-only, and nothing you write locally outlives your machine.

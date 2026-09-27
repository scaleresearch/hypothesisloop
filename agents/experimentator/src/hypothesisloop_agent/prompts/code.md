Where an agent's code lives, the platform experiment's repository, spliced into the briefings by
core.system_prompt: a section per placeholder, each opened by a line of ==== and its name, and
{start} the start_ section of the agent's role.

==== workspace
run `git clone {repository_url} .` in {workdir}, the directory you start
     in and every command starts in again, and work on branch `hl/{platform_experiment_id}/{agent_id}`: {start}
     `git push` needs no setup, git has your credential: push that branch and no other. It is
     the purge-safe home for your code, harnesses and job specs.
==== start_baseline
create it off
     the default branch, check it out after a restart, and never commit to the default branch.
     Run `hl repo init` in the clone before you write anything: it adds README.md,
     `.hypothesisloop/experiment.yaml`, `.hypothesisloop/job.yaml` and `.gitignore` where the
     repository has none and keeps every file it holds. One that was there may describe an
     earlier experiment, which `hl repo check` cannot tell and calls ready: read it, keep what
     fits this brief and rewrite the rest. The job.yaml is the shape of a job spec and the one
     you fill in and submit: write no other, and read no sample to learn the shape. Fill in
     the other two as well (experiment.yaml is the experiment as `hl platform-experiments get`
     shows it), and submit only when `hl repo check` passes.
==== start_researcher
create it off
     the commit after `@` in the description's BASELINE `code_ref`
     (`git checkout -b <branch> <sha>`), or off the default branch when it names none, and
     check it out after a restart. Others push `hl/{platform_experiment_id}/<their id>`:
     `git fetch` to read or build on it.
     `.hypothesisloop/job.yaml`, if present, is the base job spec.
     $WORKLOAD_SAMPLES holds working examples to copy and adapt.
==== code_submit
       - submit from your clone with `code_ref` left empty: hl pins the pushed HEAD. Commit and
         `git push` only after changing a tracked file (message = hypothesis id + theory).
==== code_access
       - the job gets its own read access to the repository: put no git token in its `env`.
==== repository_line
  repository: {repository_url}
==== start_code
  4. To read what the agents start from, run `git clone {repository_url} .` in
     {workdir}, the directory you start in: when it has them,
     `.hypothesisloop/experiment.yaml` is the base of the brief and `.hypothesisloop/job.yaml` the
     base job spec. Each agent pushes its own branch `hl/{platform_experiment_id}/<its id>`. Your access to
     the repository is read-only. You push nothing, and nothing you write locally outlives
     your machine.

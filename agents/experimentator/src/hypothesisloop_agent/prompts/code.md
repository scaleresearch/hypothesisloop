Where an agent's code lives, the platform experiment's repository, spliced into the briefings by
core.system_prompt: a section per placeholder, each opened by a line of ==== and its name, and
{start} the start_ section of the agent's role.

==== workspace
clone {repository_url} into the directory you start in, where every command
     starts again, and work on branch `hl/{platform_experiment_id}/{agent_id}`: {start}
     `git push` is authorized: push that branch and no other. It is the purge-safe home for
     your workload code, harnesses, job specs and Dockerfiles.
     $WORKLOAD_SAMPLES holds working examples to copy and adapt.
==== start_planner
create it off
     the default branch, and check it out after a restart.
==== start_researcher
create it off
     the commit after `@` in the description's BASELINE `code_ref`
     (`git checkout -b <branch> <sha>`), or off the default branch when it names none, and
     check it out after a restart. Others push `hl/{platform_experiment_id}/<their id>`:
     `git fetch` to read or build on it.
     `.hypothesisloop/job.yaml`, if present, is the base job spec.
==== code_submit
       - submit from your clone with `code_ref` left empty: hl pins the pushed HEAD. Commit and
         `git push` only after changing a tracked file (message = hypothesis id + theory).
==== code_access
       - the job gets its own read access to the repository: put no git token in its `env`.
==== repository_line
  repository: {repository_url}
==== start_code
  3. To read what the agents start from, clone {repository_url}: when it has them,
     `.hypothesisloop/experiment.yaml` is the base of the brief and `.hypothesisloop/job.yaml` the
     base job spec. Each agent pushes its own branch `hl/{platform_experiment_id}/<its id>`. Your access to
     the repository is read-only. You push nothing, and nothing you write locally outlives
     your machine.

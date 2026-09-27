Where an agent's code lives, the platform experiment's repository, spliced into the briefings by
core.system_prompt: a section per placeholder, each opened by a line of ==== and its name, and
{start} the start_ section of the agent's role.

==== workspace
run `git clone {repository_url} .` in {workdir}, the directory you start
     in and every command starts in again. {start}
     `git push` needs no setup, git has your credential.
==== start_baseline
Work on branch
     `hl/{platform_experiment_id}/baseline`: create it off the default branch, check it out
     after a restart, and never commit to the default branch.
     Run `hl repo init` in the clone before you write anything: it adds README.md,
     `.hypothesisloop/experiment.yaml`, `.hypothesisloop/job.yaml` and `.gitignore` where
     missing and keeps every file it holds. `hl repo check` checks the layout
     and that each file is valid, and a file that was there may still describe an earlier
     experiment: make all three match this experiment's definition before the baseline job
     (experiment.yaml is the experiment as `hl platform-experiments get` shows it). The
     job.yaml is the shape of a job spec and the one you fill in and submit: write no other,
     and read no sample to learn the shape. Submit only when `hl repo check` passes.
==== start_researcher
Every hypothesis has its
     own branch: before working on an idea, and after a restart, `hl hypothesis checkout ID`
     checks it out, started from its parent's last job commit, or from the baseline's under
     `root`. Commit there and submit: `hl job submit` pushes it and refuses any other branch.
     To drop an idea, settle it and register a new hypothesis on `root`. To improve one, yours
     or another agent's, register a child of it, which starts from its code.
     `.hypothesisloop/job.yaml`, if present, is the base job spec.
     $WORKLOAD_SAMPLES holds working examples to copy and adapt.
==== code_submit
       - submit from your clone with `code_ref` left empty: hl pins the pushed HEAD. Commit every
         change to a tracked file first (message = the theory).
==== code_access
       - the job gets its own read access to the repository: put no git token in its `env`.
==== repository_line
  repository: {repository_url}
==== start_code
  4. To read what the agents start from, run `git clone {repository_url} .` in
     {workdir}, the directory you start in: when it has them,
     `.hypothesisloop/experiment.yaml` is the base of the brief and `.hypothesisloop/job.yaml` the
     base job spec. Each hypothesis has its branch `hl/{platform_experiment_id}/<its id>`. Your access to
     the repository is read-only. You push nothing, and nothing you write locally outlives
     your machine.

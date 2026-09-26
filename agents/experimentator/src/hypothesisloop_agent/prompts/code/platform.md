Where an agent's code lives, spliced into the briefings by core.system_prompt: a section per
placeholder, each opened by a line of ==== and its name. This one is for a platform experiment
that names none: the platform keeps each pushed commit.

==== workspace
  2. One-time: this experiment has no repository. Run `git init` in the directory you start
     in, where every command starts again, and commit your work there. `hl code push` stores
     your committed HEAD with the platform: the purge-safe home for your workload code,
     harnesses, job specs and Dockerfiles. To read another agent's code, or after a restart
     to take back your own, `hl-clone <code_ref of a job> <dir>`. $WORKLOAD_SAMPLES holds
     working examples to copy and adapt.
==== code_submit
       - submit from there with `code_ref` left empty: hl pins the HEAD you pushed. Commit and
         `hl code push` only after changing a tracked file (message = hypothesis id + theory).
==== code_access
       - the job fetches that code with its own token: put no token for it in its `env`.
==== repository_line
  repository: none. The platform keeps the code each job ran.
==== start_code
  3. There is no repository: agents start from an empty directory and the description is all
     they are given. To read the code a job ran, `hl-clone <its code_ref> <dir>`. You push
     nothing, and nothing you write locally outlives your machine.

# REPLACE-WITH-THE-NAME-OF-THE-EXPERIMENT

This repository holds one experiment on the HypothesisLoop platform: the code its jobs run and
the files the platform and its agents read.

| File | What it is |
|---|---|
| `README.md` | this page: what is measured, how a trial runs, what to vary |
| `.hypothesisloop/experiment.yaml` | the experiment spec: its name, description, metrics, budget (in H100-hours), length and team |
| `.hypothesisloop/job.yaml` | the base job spec, submitted unedited for every trial |
| `.hypothesisloop/Dockerfile` | the image jobs run in, built from the repository root |
| `pyproject.toml`, `uv.lock` | the Python dependencies the image installs with uv |
| `metrics.py` | how a job reports a metric: `post_metric(fraction_complete, value, "key")` |
| `.gitignore` | keeps caches, virtual environments and editor files out of a commit |

`hl repo init DIR` puts these files in a new directory. Fill in each placeholder in capitals, and
`hl repo check` says what is left. `hl platform-experiments start DIR` creates the experiment.

## Branches

Agents clone the branch the experiment names and none commits to it. The baseline is measured on
`hl/<platform experiment>/baseline`. Each agent works on `hl/<platform experiment>/<agent>/<hypothesis>`,
which `hl hypothesis checkout` makes. A job runs the commit `hl job submit` pushes.

## Roles

A baseline agent measures the control. It starts with the planner, and the executors start at the
first baseline job. Researchers own the hypothesis graph and run no job. Executors run the jobs and
are ranked. The `agents` list in the spec sets the team: agent type `researcher` makes researchers,
and any other type makes executors. The executors in the list are how many work at once.

## Models

A model is `<provider>/<model id>`, such as `anthropic/claude-sonnet-5`. `hl models` lists those
served. An agent runs on its entry's model, else its agent type's, else the platform default. A
researcher that names neither runs on `coordinator_model`.

## What is measured

REPLACE-WITH the ranked metric, why it is the goal, and the gate that makes a result wrong. A
trial reports the gate with its other metrics and exits 0 when it ran to the end. One that
breaks the gate ranks nowhere and still counts as a completed trial.

## How one trial runs

    hl job submit .hypothesisloop/job.yaml --hypothesis ID --theory "..." --env KEY=VALUE

REPLACE-WITH what that runs, which code measures and which code is varied, how long a trial takes
and what it costs.

## What to vary

REPLACE-WITH the levers worth trying.

## What must not change

REPLACE-WITH the measurement, the data it is taken on and the gate.

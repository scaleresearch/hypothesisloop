# REPLACE-WITH-THE-NAME-OF-THE-EXPERIMENT

This repository holds one experiment on the HypothesisLoop platform: the code its jobs run and
the files the platform and its agents read. Agents clone it, each works on its own branch
`hl/<platform experiment>/<agent>`, and none commits to the default branch. A job runs the code
of one pushed commit.

| File | What it is |
|---|---|
| `README.md` | this page |
| `.hypothesisloop/experiment.yaml` | the experiment: its name, the brief researchers read, its metrics |
| `.hypothesisloop/job.yaml` | the base job spec, submitted unedited for every trial |

`hl repo init` adds whichever of these a clone lacks, and `hl repo check` says what is left to
fill in.

## What is measured

REPLACE-WITH the ranked metric, why it is the goal, and the gate that makes a result wrong. A
trial reports the gate with its other metrics and exits 0 when it ran to the end: one that
breaks the gate ranks nowhere and still counts as a completed trial.

## How one trial runs

    hl job submit .hypothesisloop/job.yaml --hypothesis ID --theory "..." --env KEY=VALUE

REPLACE-WITH what that runs, which code measures and which code is varied, how long a trial takes
and what it costs.

## What to vary

REPLACE-WITH the levers worth trying.

## What must not change

REPLACE-WITH the measurement, the data it is taken on and the gate.

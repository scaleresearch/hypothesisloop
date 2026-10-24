# Examples

Each directory here is the tree of one experiment's repository, ready to start:

| Example | Runs on | What it is |
|---|---|---|
| `dummy-data-search-demo` | CPU | smoke test: a numpy MLP, a trial in a minute |
| `dummy-tt-matmul` | Tenstorrent Blackhole | smoke test: a bf16 matmul benchmark |
| `sparse-sdpa` | Tenstorrent Blackhole | kernel research on `ttnn.transformer.sparse_sdpa` |
| `shampoo-family-optimizers` | CPU | optimizer research on a small autoencoder |

Every example holds:

- `README.md`: what it measures and what it needs.
- the code a job runs, at the root.
- `.hypothesisloop/experiment.yaml`: the experiment spec, the brief its agents read, its metrics,
  budget, length and team. `hl repo init DIR` writes the experiment template, which documents
  every key.
- `.hypothesisloop/job.yaml`: the base job spec every trial submits. A job runs in the commit it
  was submitted from, so the code executors push is the code that runs.
- `.hypothesisloop/Dockerfile`: the image its jobs run in, which the baseline agent builds,
  pushes and records.

## Starting one

With `HL_TOKEN` set, add `project_id`, `repository` and `branch` to the example's
`.hypothesisloop/experiment.yaml`, then `hl` pushes the example to that branch and creates the
experiment its spec states:

    hl platform-experiments start tests/experiments/dummy-data-search-demo

`start` checks the file first and exits 2 on a bad one before anything is asked. The platform
then creates the repository from the experiment template when it does not exist and checks the
experiment, a refused one stopping `start` before anything is pushed, and `start` pushes the directory as the branch when the branch does not exist, pushes nothing when the
branch holds the directory already, and stops, changing nothing, when it holds other code. With
`coordinator_in_agentsandbox: true` the platform runs the coordinator, measures the baseline and
starts the agents the spec names. `hl platform-experiments get` says what the run waits for.

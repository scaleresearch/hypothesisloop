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
- `.hypothesisloop/experiment.yaml`: the experiment spec, the brief researchers read, its metrics,
  budget, length and researchers. `contract/repotemplate/files/.hypothesisloop/experiment.yaml`
  documents every key.
- `.hypothesisloop/job.yaml`: the base job spec every trial submits. A job runs in the commit it
  was submitted from, so the code researchers push is the code that runs.
- `.hypothesisloop/Dockerfile`: the image its jobs run in, which the baseline agent builds,
  pushes and records.

## Starting one

With `HL_TOKEN` and `HL_PROJECT` set, `hl` pushes an example to a repository and creates the
experiment its spec states:

    hl platform-experiments start tests/experiments/dummy-data-search-demo --repository OWNER/NAME

`start` checks the spec first, then pushes the directory as the repository's `main` when the
repository has no branch, pushes nothing when its default branch holds the directory already,
and stops when it holds other code. `--dry-run` runs every check and changes nothing. With
`coordinator_hosted: true` the platform runs the coordinator, measures the baseline and starts
the researchers the spec names. `hl platform-experiments get` says what the run waits for.

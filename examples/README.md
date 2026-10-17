# Examples

Each directory here is the tree of one experiment's repository, ready to start:

| Example | Runs on | What it is |
|---|---|---|
| `dummy-data-search-demo` | CPU, `python` job image | smoke test: a numpy MLP, a trial in a minute |
| `dummy-tt-matmul` | Tenstorrent Blackhole, `tenstorrent` job image | smoke test: a bf16 matmul benchmark |
| `sparse-sdpa` | Tenstorrent Blackhole, `tenstorrent` job image | kernel research on `ttnn.transformer.sparse_sdpa` |
| `shampoo-family-optimizers` | CPU, `python` job image | optimizer research on a small autoencoder |

Every example holds:

- `README.md`: what it measures and what it needs.
- the code a job runs, at the root.
- `.hypothesisloop/experiment.yaml`: the experiment spec, the brief researchers read, its metrics,
  budget, length and researchers. `contract/repotemplate/files/.hypothesisloop/experiment.yaml`
  documents every key.
- `.hypothesisloop/job.yaml`: the base job spec every trial submits. A job clones the commit it was
  submitted from with `hl-clone` and runs it, so the code researchers push is the code that runs.

## Starting one

Push the example as the default branch of a repository a GitHub account connected to your org
covers, then create the experiment on it:

    cd examples/dummy-data-search-demo
    git init -b main && git add -A && git commit -m "dummy-data-search-demo"
    gh repo create OWNER/NAME --private --source . --push
    { cat .hypothesisloop/experiment.yaml; echo "repository: OWNER/NAME"; } \
      | hl platform-experiments create --project PROJECT -

With `coordinator_hosted: true` the platform runs the coordinator, measures the baseline and
starts the researchers the spec names. `hl platform-experiments get` says what the run waits for.

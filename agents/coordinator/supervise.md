# Coordinator: supervise a run from a host

For after `setup.md` has agents running, with the same variables.

How a run is supervised is written once, in the coordinator briefing
`agents/experimentator/src/hypothesisloop_agent/prompts/coordinator.md`: what to watch, the
brief, the baseline, the flavor mix, blockers, the summary and the close. Read it and work from
it with your own token and `hl`. This file holds only the steps that need a host, which a
coordinator the platform runs (`coordinator_hosted`) has none of.

You watch. You never act on an agent's behalf.

## Agents you run yourself

Every 5 to 10 minutes at first, less often once the run is steady:

- `podman ps`: a self-run agent whose container is gone has exited. Read its `.err`.
- `podman logs --tail 100 agent-<n>`, or the tail of its `.activity.log`: progress or a loop.
  `setup.md` section 4 lists the kinds of line.

With an API key every command below that works in the platform experiment states
`--platform-experiment $PLATFORM_EXPERIMENT_ID`: `hl` reads it from no variable.

A hosted agent needs neither:
`hl platform-experiments agents list --platform-experiment $PLATFORM_EXPERIMENT_ID` shows its
machine (`instance`), and
`hl platform-experiments agents session --platform-experiment $PLATFORM_EXPERIMENT_ID AGENT`
writes out what it said and saw.

`hl job list --platform-experiment $PLATFORM_EXPERIMENT_ID` prints one brief line per job, 20 a
page and 50 at most (`--limit`, `--offset`), and says how many more there are. `hl job status ID` prints a job's whole record, so ask it
only for the jobs you need.

## The brief and its files

The live `description` is the only place an agent reads the question. On a host the
`EXPERIMENT DESCRIPTION` block is also kept in `experiment.md` and
`.hypothesisloop/experiment.yaml`, so an edit to either file is finished only once
`hl platform-experiments update --platform-experiment $PLATFORM_EXPERIMENT_ID --description-file
FILE` has sent the block and the description it answers with matches the file (`setup.md` step 2). The update tells every agent that the
brief changed, with a `platform_experiment.description` event that carries no text.

## Blockers a host can fix

Capacity, an image, the repository, platform code: `setup.md`, "Fixing a blocker". When the
platform experiment cannot be recovered, write why into the summary, close it and start a new
one.

## Findings

The run's findings go into the summary, as the briefing says. What concerns the platform or
the next run's setup goes into `$FINDINGS_FILE` as one plain line under the heading it belongs
to. Add a line only for a new fact, and delete one that is done.

## End the run

- Write the summary (`hl platform-experiments summary --platform-experiment
  $PLATFORM_EXPERIMENT_ID --file FILE`), then
  `hl platform-experiments close --platform-experiment $PLATFORM_EXPERIMENT_ID`.
- `lib_detach_node $KUBE_CONTEXT $NODE_NAME`.
- `podman stop -t 90 agent-<n>` for each agent you run yourself: the agent takes one last turn
  of up to a minute to file owed summaries and verdicts. A hosted agent needs nothing: its token
  works for two more minutes and its machine is taken down after them. Leave the branches and
  commits in the experiment's repository.

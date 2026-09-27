import { addPlatformExperimentAgents, createPlatformExperiment, updatePlatformExperiment } from '@/lib/api'
import type { CreatePlatformExperimentRequest } from '@/lib/api'
import type { PlatformExperiment } from '@/types'

/** A run that stopped part way: the experiment exists, and this step is the one the API refused. */
export interface RunFailure {
  step: string
  message: string
}

export interface RunOutcome {
  experiment: PlatformExperiment
  failure?: RunFailure
}

const failures = new Map<string, RunFailure>()

/** What stopped the run of an experiment created in this tab, for its page to show. */
export function runFailure(experimentID: string): RunFailure | undefined {
  return failures.get(experimentID)
}

export function clearRunFailure(experimentID: string): void {
  failures.delete(experimentID)
}

const said = (error: unknown) => (error instanceof Error ? error.message : String(error))

/**
 * Creates a platform experiment the platform runs: the experiment with a hosted coordinator and
 * no start time, so that nothing starts it between two agents joining, then one hosted agent a
 * call, then its start time, now. The platform starts it from then, once its coordinator has had
 * the baseline measured and declared it. The calls are made in order and one at a time. A refused create rejects and nothing exists. Any
 * later refusal ends the run where it is: nothing is undone or tried again, and the outcome names
 * the step, for the experiment's page to finish from.
 */
export async function createAndRun(projectID: string, request: CreatePlatformExperimentRequest, researchers: number): Promise<RunOutcome> {
  const { starts_at: _unscheduled, ...unscheduled } = request
  const experiment = await createPlatformExperiment(projectID, { ...unscheduled, coordinator_hosted: true })
  const stopped = (step: string, error: unknown): RunOutcome => {
    const failure = { step, message: said(error) }
    failures.set(experiment.id, failure)
    return { experiment, failure }
  }
  for (let n = 1; n <= researchers; n++) {
    try {
      await addPlatformExperimentAgents(experiment.id, { hosted: true })
    } catch (error) {
      return stopped(`adding hosted agent ${n} of ${researchers}`, error)
    }
  }
  try {
    await updatePlatformExperiment(experiment.id, { starts_at: new Date().toISOString() })
  } catch (error) {
    return stopped('setting its start time', error)
  }
  return { experiment }
}

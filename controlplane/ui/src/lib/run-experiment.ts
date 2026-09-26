import { addPlatformExperimentAgents, createPlatformExperiment, startPlatformExperiment } from '@/lib/api'
import type { CreatePlatformExperimentRequest } from '@/lib/api'
import type { PlatformExperiment } from '@/types'

// Go's zero time. As starts_at it states "no scheduled start": the platform starts such an
// experiment on no clock of its own, so it cannot start between two agents joining.
const NO_SCHEDULED_START = '0001-01-01T00:00:00Z'

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
 * Creates a platform experiment the platform runs, and starts it: the experiment with a hosted
 * coordinator, then one hosted agent a call, then the start. They are the calls the experiment's
 * own page makes, in order and one at a time. A refused create rejects and nothing exists. Any
 * later refusal ends the run where it is: nothing is undone or tried again, and the outcome names
 * the step, for the experiment's page to finish from.
 */
export async function createAndRun(projectID: string, request: CreatePlatformExperimentRequest, researchers: number): Promise<RunOutcome> {
  const experiment = await createPlatformExperiment(projectID, { ...request, starts_at: NO_SCHEDULED_START, coordinator_hosted: true })
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
    await startPlatformExperiment(experiment.id)
  } catch (error) {
    return stopped('starting it', error)
  }
  return { experiment }
}

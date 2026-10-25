'use client'

import { useState } from 'react'
import { useRouter } from 'next/navigation'
import { formatDate, formatDuration } from '@/lib/format'
import { reasonOf } from '@/lib/phase'
import { formDraftFromGoal, PLANNING_MOVES } from '@/lib/planning'
import { DEFAULT_BRANCH } from '@/components/repository-select'
import { projectPath } from '@/lib/routes'
import { useTenant } from '@/lib/tenant'
import { useTick } from '@/lib/tick'
import { AgentSandboxNotice } from '@/components/agentsandbox-notice'
import { Button } from '@/components/ui/button'
import { Stack } from '@/components/ui/stack'
import { InlineError } from '@/components/ui/status-message'
import { Text } from '@/components/ui/text'
import { errorMessage } from '@/lib/errors'
import type { JoinedAgent, PlatformExperiment } from '@/types/api'


/** How many times the platform restarts a planner that dies (domain.MaxAgentInstanceRestarts). */
const MAX_RESTARTS = 3

/** What is happening right now, in a few words, from the draft's reason, its planner's machine and what the planner last said. */
function nowDoing(pe: PlatformExperiment, planner: JoinedAgent | undefined): string {
  if (reasonOf(pe).reason === 'repository_creating') return 'Creating your repository…'
  const instance = planner?.instance
  if (instance?.state !== 'running') {
    if (!instance || instance.restarts === 0) return 'Starting the planner…'
    const attempt = `Trying again (${Math.min(instance.restarts, MAX_RESTARTS)} of ${MAX_RESTARTS}).`
    return instance.exit_reason ? `The planner stopped at start: ${instance.exit_reason}. ${attempt}` : `Restarting the planner. ${attempt}`
  }
  return pe.activity?.text ?? 'The planner is reading your repository…'
}

/** Keeps the draft's goal as a draft of the full form and opens it there. */
function SetUpMyself({ pe }: { pe: PlatformExperiment }) {
  const router = useRouter()
  const { project } = useTenant()
  const [moving, setMoving] = useState(false)
  const [error, setError] = useState<string | null>(null)
  async function go() {
    setMoving(true)
    setError(null)
    try {
      const form = await formDraftFromGoal(project.id, pe.goal ?? '', pe.repository ?? '', pe.branch || DEFAULT_BRANCH)
      router.push(`${projectPath(project.id, 'experiments')}?draft=${form.id}`)
    } catch (e) {
      setError(`Could not open the form: ${errorMessage(e)}`)
      setMoving(false)
    }
  }
  return (
    <>
      <Button size="sm" loading={moving} onClick={() => void go()}>Set up manually</Button>
      {error && <InlineError>{error}</InlineError>}
    </>
  )
}

/**
 * A draft while it is planned: what is happening right now and for how long, or, while something
 * holds planning up, what that is in the platform's words, what it means for the person and what
 * they can do.
 */
export function PlanningProgress({ pe, planner }: { pe: PlatformExperiment; planner: JoinedAgent | undefined }) {
  useTick(1_000)
  const reason = reasonOf(pe)
  const revising = reason.reason === 'revising'
  const elapsed = formatDuration(Math.max(0, Date.now() - new Date(pe.created_at).getTime()))

  if (!PLANNING_MOVES.has(reason.reason)) {
    return (
      <AgentSandboxNotice
        reason={reason}
        title="Planning is on hold"
        meaning={(
          <>
            Planning starts once the platform can run it.
            {pe.planning_ends_at && ` If it cannot start by ${formatDate(pe.planning_ends_at)}, the draft closes.`}
          </>
        )}
        actions={pe.actions.includes('abandon') && <SetUpMyself pe={pe} />}
      />
    )
  }

  return (
    <Stack gap="sm">
      <Text className="text-base font-medium" role="status" aria-live="polite">
        {revising ? 'Updating the plan with your change. ' : ''}{nowDoing(pe, planner)}
      </Text>
      {!revising && (
        <Text variant="note">
          <span className="tabular-nums">{elapsed}</span> so far. You can leave this page.
        </Text>
      )}
    </Stack>
  )
}

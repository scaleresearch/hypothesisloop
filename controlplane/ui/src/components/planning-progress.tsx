'use client'

import { useState } from 'react'
import { useRouter } from 'next/navigation'
import { formatDate, formatDuration } from '@/lib/format'
import { reasonOf } from '@/lib/phase'
import { formDraftFromGoal, PLANNING_MOVES } from '@/lib/planning'
import { projectPath } from '@/lib/routes'
import { useTenant } from '@/lib/tenant'
import { useTick } from '@/lib/tick'
import { HostingNotice } from '@/components/hosting-notice'
import { Button } from '@/components/ui/button'
import { Stack } from '@/components/ui/stack'
import { InlineError } from '@/components/ui/status-message'
import { Steps } from '@/components/ui/steps'
import { Text } from '@/components/ui/text'
import { errorMessage } from '@/lib/errors'
import type { JoinedAgent, PlatformExperiment } from '@/types/api'


const STEPS = [
  { key: 'starting', label: 'Starting the planner' },
  { key: 'planning', label: 'Planning' },
  { key: 'proposal', label: 'Plan ready for you' },
]

/** What is happening right now, in a few words, from the draft's reason, its planner's machine and what the planner last said. */
function nowDoing(pe: PlatformExperiment, planner: JoinedAgent | undefined): { step: 'starting' | 'planning'; text: string } {
  if (reasonOf(pe).reason === 'repository_creating') return { step: 'starting', text: 'Creating your repository…' }
  const instance = planner?.instance
  if (instance?.state !== 'running') return { step: 'starting', text: instance && instance.restarts > 0 ? 'Restarting the planner…' : 'Starting the planner…' }
  return { step: 'planning', text: pe.activity?.text ?? 'The planner is reading your repository…' }
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
      const form = await formDraftFromGoal(project.id, pe.goal ?? '', pe.repository ?? '')
      router.push(`${projectPath(project.id, 'experiments')}?draft=${form.id}`)
    } catch (e) {
      setError(`Could not open the form: ${errorMessage(e)}`)
      setMoving(false)
    }
  }
  return (
    <>
      <Button size="sm" loading={moving} onClick={() => void go()}>Set it up myself</Button>
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
      <HostingNotice
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

  const now = nowDoing(pe, planner)
  return (
    <Stack gap="sm">
      <Steps label="Planning" steps={STEPS} current={now.step} live />
      <Text className="text-base font-medium" role="status" aria-live="polite">
        {revising ? 'Updating the plan with your change. ' : ''}{now.text}
      </Text>
      {!revising && (
        <Text variant="note">
          <span className="tabular-nums">{elapsed}</span> so far.
        </Text>
      )}
    </Stack>
  )
}

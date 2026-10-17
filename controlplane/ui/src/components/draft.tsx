'use client'

import { useState } from 'react'
import { useSWRConfig } from 'swr'
import { formatDate } from '@/lib/format'
import { abandonDraft } from '@/lib/api/planning'
import { PLANNING_MOVES, proposalOf } from '@/lib/planning'
import { reasonOf } from '@/lib/phase'
import { useRouter } from 'next/navigation'
import { projectPath } from '@/lib/routes'
import { useTenant } from '@/lib/tenant'
import { GoalEditor } from '@/components/goal-editor'
import { PhaseStrip } from '@/components/phase-strip'
import { ProposalForm } from '@/components/proposal-form'
import { Button } from '@/components/ui/button'
import { ConfirmDialog } from '@/components/ui/dialog'
import { Page } from '@/components/ui/page'
import { Section } from '@/components/ui/section'
import { Inline, Stack } from '@/components/ui/stack'
import { Text } from '@/components/ui/text'
import { keys } from '@/lib/keys'
import type { PlatformExperiment } from '@/types/api'

/**
 * The page of an experiment that is still a draft: the goal it is planned from, what its planner
 * is doing or proposed, and what a person does with a proposal: let the platform accept it as
 * proposed, accept it now, edit it and accept it at a cost they state, change the goal, or reject
 * it. A reader of a shared draft is shown none of these.
 */
export function DraftView({ draft }: { draft: PlatformExperiment }) {
  const router = useRouter()
  const { project } = useTenant()
  const { mutate } = useSWRConfig()
  const [confirming, setConfirming] = useState(false)
  const state = reasonOf(draft).reason
  const proposal = proposalOf(draft)
  const accepts = draft.actions.includes('accept')
  const reread = () => void mutate(keys.experiment(draft.id))

  async function abandon() {
    await abandonDraft(draft.id)
    reread()
  }

  return (
    <Page
      back={{ label: 'Experiments', onClick: () => router.push(projectPath(project.id, 'experiments')) }}
      title={draft.name}
      description={<Text as="span" variant="hint">Draft, {state === 'proposed' || state === 'declined' ? 'waiting for you' : state === 'accepting' ? 'starting in a moment' : PLANNING_MOVES.has(state) ? 'being planned' : 'planning on hold'}</Text>}
    >
      <PhaseStrip pe={draft} />
      <GoalEditor pe={draft} onSaved={reread} />
      {draft.declined_reason && (
        <Section title="What the planner needs">
          <Stack gap="sm">
            <Text>{draft.declined_reason}</Text>
            {state === 'declined' && (
              <Text variant="note">
                Edit what you asked and the planner tries again. Otherwise the draft closes on {formatDate(draft.proposal_expires_at)}.
              </Text>
            )}
          </Stack>
        </Section>
      )}
      {proposal && (
        <ProposalForm key={draft.proposed_at} draft={draft} proposal={proposal} editable={accepts && state === 'proposed'} accepting={accepts && state === 'accepting'} onDone={reread} />
      )}
      {draft.actions.includes('abandon') && (
        <Inline gap="md">
          <Button variant="ghost" onClick={() => setConfirming(true)}>Abandon this draft</Button>
          <ConfirmDialog open={confirming} title="Abandon this draft?" confirmLabel="Abandon draft" onConfirm={abandon} onClose={() => setConfirming(false)}>
            Planning stops and the draft closes.
          </ConfirmDialog>
        </Inline>
      )}
    </Page>
  )
}

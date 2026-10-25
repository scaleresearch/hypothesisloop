'use client'

import { useState } from 'react'
import { formatDate } from '@/lib/format'
import { abandonDraft } from '@/lib/api/planning'
import { reasonOf } from '@/lib/phase'
import { GoalEditor } from '@/components/goal-editor'
import { PhaseStrip } from '@/components/phase-strip'
import { Plan } from '@/components/plan'
import { Button } from '@/components/ui/button'
import { ConfirmDialog } from '@/components/ui/dialog'
import { Prose } from '@/components/ui/prose'
import { Section } from '@/components/ui/section'
import { Stack } from '@/components/ui/stack'
import { Text } from '@/components/ui/text'
import type { PlatformExperiment } from '@/types/api'

/** What a draft's page shows: where it stands, what was asked, and the plan as the planner drafts it. */
export function DraftSections({ pe, onChanged }: { pe: PlatformExperiment; onChanged: () => void }) {
  return (
    <>
      <PhaseStrip pe={pe} />
      <GoalEditor pe={pe} onSaved={onChanged} />
      <Declined pe={pe} />
      <Plan pe={pe} />
    </>
  )
}

/** Why the planner proposed nothing, and what the person does about it. */
function Declined({ pe }: { pe: PlatformExperiment }) {
  if (!pe.declined_reason) return null
  return (
    <Section title="What the planner needs">
      <Stack gap="sm">
        <Prose collapsedLines={6} text={pe.declined_reason} />
        {reasonOf(pe).reason === 'declined' && (
          <Text variant="note">
            Edit what you asked and the planner tries again. Otherwise the draft closes on {formatDate(pe.proposal_expires_at)}.
          </Text>
        )}
      </Stack>
    </Section>
  )
}

/** The owner's control to stop planning a draft, behind a confirmation. */
export function AbandonDraft({ pe, onAbandoned }: { pe: PlatformExperiment; onAbandoned: () => void }) {
  const [asking, setAsking] = useState(false)
  return (
    <>
      <Button size="sm" onClick={() => setAsking(true)}>Abandon draft</Button>
      <ConfirmDialog
        open={asking}
        title="Abandon this draft?"
        confirmLabel="Abandon draft"
        onConfirm={async () => {
          await abandonDraft(pe.id)
          onAbandoned()
        }}
        onClose={() => setAsking(false)}
      >
        Planning stops and the draft closes.
      </ConfirmDialog>
    </>
  )
}

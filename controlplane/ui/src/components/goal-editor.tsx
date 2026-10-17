'use client'

import { useState } from 'react'
import { updatePlatformExperiment } from '@/lib/api/platform-experiments'
import { errorMessage } from '@/lib/errors'
import { MAX_GOAL_LENGTH } from '@/lib/planning'
import { canEdit, PlatformExperimentStatus } from '@/types/platform-experiments'
import type { PlatformExperiment } from '@/types/platform-experiments'
import { Button } from '@/components/ui/button'
import { FormField } from '@/components/ui/form-field'
import { Textarea } from '@/components/ui/input'
import { Section } from '@/components/ui/section'
import { Inline, Stack } from '@/components/ui/stack'
import { InlineError } from '@/components/ui/status-message'
import { Text } from '@/components/ui/text'

/**
 * What the owner asked, which they change while the API lists goal in editable: a draft is
 * planned again from it, and the coordinator of an open or running one brings the brief in line.
 */
export function GoalEditor({ pe, onSaved }: { pe: PlatformExperiment; onSaved: () => void }) {
  const [goal, setGoal] = useState<string | null>(null)
  const [saving, setSaving] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const draft = pe.status === PlatformExperimentStatus.DRAFT

  async function save() {
    setSaving(true)
    setError(null)
    try {
      await updatePlatformExperiment(pe.id, { goal: goal!.trim() })
      setGoal(null)
      onSaved()
    } catch (e) {
      setError(errorMessage(e))
    }
    setSaving(false)
  }

  return (
    <Section
      title="What you asked"
      actions={goal === null && canEdit(pe, 'goal') && <Button size="sm" aria-label="Edit what you asked" onClick={() => setGoal(pe.goal ?? '')}>Edit</Button>}
    >
      {goal === null ? (
        <Text className="whitespace-pre-line">{pe.goal}</Text>
      ) : (
        <Stack gap="sm">
          <FormField
            label="Goal"
            hint={draft ? 'Saving starts planning again from this text.' : 'The coordinator is told and updates the brief.'}
          >
            <Textarea rows={4} maxLength={MAX_GOAL_LENGTH} value={goal} onChange={e => setGoal(e.target.value)} />
          </FormField>
          <Inline gap="md">
            <Button variant="primary" loading={saving} disabled={goal.trim() === '' || goal.trim() === pe.goal} onClick={() => void save()}>Save</Button>
            <Button disabled={saving} onClick={() => { setGoal(null); setError(null) }}>Cancel</Button>
            {error && <InlineError>{error}</InlineError>}
          </Inline>
        </Stack>
      )}
    </Section>
  )
}

'use client'

import { useState } from 'react'
import useSWR from 'swr'
import { deleteExperimentDraft, fetchExperimentDrafts } from '@/lib/api/drafts'
import { headline, relTime } from '@/lib/format'
import { useTenant } from '@/lib/tenant'
import { Button } from '@/components/ui/button'
import { ConfirmDialog } from '@/components/ui/dialog'
import { Section } from '@/components/ui/section'
import { Inline, Stack } from '@/components/ui/stack'
import { ErrorMessage, InlineError } from '@/components/ui/status-message'
import { Text } from '@/components/ui/text'
import { keys } from '@/lib/keys'
import type { ExperimentDraft } from '@/types/api'

// The name typed, else the start of the brief, so that a draft kept from a goal reads as that goal.
function nameOf(draft: ExperimentDraft): string {
  const form = draft.form as Record<string, unknown>
  const name = typeof form.name === 'string' ? form.name.trim() : ''
  const brief = typeof form.description === 'string' ? form.description.trim() : ''
  return name || (brief && headline(brief, 80)) || 'Unnamed experiment'
}

/**
 * The forms of new experiments the project keeps to finish later, the one changed last first, each
 * with why its last create was refused when one was. Nothing while there are none.
 */
export function ExperimentDrafts({ onOpen }: { onOpen: (draft: ExperimentDraft) => void }) {
  const { project } = useTenant()
  const { data, error, mutate } = useSWR(keys.experimentDrafts(project.id), () => fetchExperimentDrafts(project.id))
  const [deleting, setDeleting] = useState<ExperimentDraft | null>(null)

  async function drop(draft: ExperimentDraft) {
    await deleteExperimentDraft(project.id, draft.id)
    await mutate()
  }

  if (error) return <ErrorMessage>Could not load the drafts of {project.name}: {error.message}</ErrorMessage>
  if (!data || data.length === 0) return null
  return (
    <Section title="Saved drafts" note={data.length}>
      <Stack gap="sm">
        {data.map(draft => (
          <Inline key={draft.id} nowrap>
            <Stack gap="xs" className="min-w-0 flex-1">
              <Text className="truncate font-medium">{nameOf(draft)}</Text>
              <Text variant="hint">Saved {relTime(draft.updated_at)}</Text>
              {draft.failure && <InlineError>Not created: {draft.failure}</InlineError>}
            </Stack>
            <Button size="sm" onClick={() => onOpen(draft)}>Continue</Button>
            <Button size="sm" variant="ghost" onClick={() => setDeleting(draft)}>Delete</Button>
          </Inline>
        ))}
        {deleting && (
          <ConfirmDialog
            open
            title="Delete this draft?"
            confirmLabel="Delete draft"
            onConfirm={() => drop(deleting)}
            onClose={() => setDeleting(null)}
          >
            {`"${nameOf(deleting)}" and everything typed in it are deleted.`}
          </ConfirmDialog>
        )}
      </Stack>
    </Section>
  )
}

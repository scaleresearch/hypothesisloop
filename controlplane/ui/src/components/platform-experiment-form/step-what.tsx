import { RepositorySelect } from '@/components/repository-select'
import { FieldGroup, FormField } from '@/components/ui/form-field'
import { InfoTip } from '@/components/ui/info-tip'
import { Input, Textarea } from '@/components/ui/input'
import { InlineError } from '@/components/ui/status-message'
import { Text } from '@/components/ui/text'
import type { StepProps } from '@/components/platform-experiment-form/model'

/** What the chosen repository's experiment file did to the form: is being read, filled it, is not there, or could not be read. */
export type FileNote = { repository: string; state: 'reading' | 'filled' | 'none' | 'failed'; message?: string }

const FILE = '.hypothesisloop/experiment.yaml'

/** Step 1: the code, the name and the brief. */
export function StepWhat({ form, set, isEdit, fileNote, onRepository }: StepProps & {
  fileNote: FileNote | null
  onRepository: (repository: string) => void
}) {
  return (
    <>
      {isEdit ? (
        <FieldGroup label="Repository and agent image" hint="Fixed when the experiment was created.">
          <Text>{form.repository || 'No repository'} · {form.agent_image || 'the platform\'s agent image'}</Text>
        </FieldGroup>
      ) : (
        <FormField
          label="Repository"
          hint={<InfoTip>{`A repository with ${FILE} fills in the rest of this form.`}</InfoTip>}
        >
          <RepositorySelect value={form.repository} onChange={onRepository} />
          {fileNote?.state === 'reading' && <Text variant="hint">Reading {FILE} of {fileNote.repository}…</Text>}
          {fileNote?.state === 'filled' && <Text variant="note">Filled in from {FILE} of {fileNote.repository}.</Text>}
          {fileNote?.state === 'none' && <Text variant="note">{fileNote.repository} has no {FILE}.</Text>}
          {fileNote?.state === 'failed' && <InlineError>Could not read {FILE} of {fileNote.repository}: {fileNote.message}</InlineError>}
        </FormField>
      )}
      <FormField label="Name">
        <Input value={form.name} onChange={e => set('name', e.target.value)} placeholder="e.g. Q3 Vision Benchmark" />
      </FormField>
      <FormField label="Description" hint="The brief every agent reads before choosing an approach.">
        <Textarea
          rows={4}
          className="overflow-x-hidden break-words"
          value={form.description}
          onChange={e => set('description', e.target.value)}
          placeholder="Goals, evaluation protocol, dataset details, winner criteria…"
        />
      </FormField>
    </>
  )
}

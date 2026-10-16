import { useRef } from 'react'
import { BriefPoints } from '@/components/brief-points'
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

const BRIEF_PLACEHOLDER = `Objective: what to find out, in a sentence.
Metric: …
Control: …
Win: …`

/** Step 1: the code, the name and the brief. */
export function StepWhat({ form, set, isEdit, fileNote, onRepository, leave }: StepProps & {
  fileNote: FileNote | null
  onRepository: (repository: string) => void
  /** Keeps the form and answers where GitHub's return comes back to. */
  leave: () => Promise<string>
}) {
  const brief = useRef<HTMLTextAreaElement>(null)
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
          <RepositorySelect value={form.repository} onChange={onRepository} leave={leave} />
          {fileNote?.state === 'reading' && <Text variant="hint">Reading {FILE} of {fileNote.repository}…</Text>}
          {fileNote?.state === 'filled' && <Text variant="note">Filled in from {FILE} of {fileNote.repository}.</Text>}
          {fileNote?.state === 'none' && <Text variant="note">{fileNote.repository} has no {FILE}.</Text>}
          {fileNote?.state === 'failed' && <InlineError>Could not read {FILE} of {fileNote.repository}: {fileNote.message}</InlineError>}
        </FormField>
      )}
      <FormField label="Name">
        <Input value={form.name} onChange={e => set('name', e.target.value)} placeholder="e.g. Q3 Vision Benchmark" />
      </FormField>
      <FormField
        label="Brief"
        hint={<>The only text every agent reads about the task. Whatever it leaves open, agents guess from the code.<InfoTip>Write it so that anyone could check a result against it, with numbers, units and file names.</InfoTip></>}
      >
        <Textarea
          ref={brief}
          rows={8}
          className="overflow-x-hidden break-words"
          value={form.description}
          onChange={e => set('description', e.target.value)}
          placeholder={BRIEF_PLACEHOLDER}
        />
      </FormField>
      <BriefPoints text={form.description} onChange={text => set('description', text)} box={brief} />
    </>
  )
}

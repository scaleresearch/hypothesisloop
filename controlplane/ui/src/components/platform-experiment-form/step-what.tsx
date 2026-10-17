import { BriefGuide } from '@/components/brief-guide'
import { namesRepository, RepositorySelect } from '@/components/repository-select'
import { Button } from '@/components/ui/button'
import { FieldGroup, FormField } from '@/components/ui/form-field'
import { Input, Textarea } from '@/components/ui/input'
import { Inline } from '@/components/ui/stack'
import { InlineError } from '@/components/ui/status-message'
import { Text } from '@/components/ui/text'
import type { StepProps } from '@/components/platform-experiment-form/model'

/**
 * What the experiment file at the chosen repository and branch did to the form: is being read,
 * filled the fields named, is not there, places the experiment elsewhere, or could not be read.
 */
export type FileNote =
  | { repository: string; branch: string; state: 'reading' | 'none' }
  | { repository: string; branch: string; state: 'filled'; filled: string[] }
  | { repository: string; branch: string; state: 'elsewhere' | 'failed'; message: string }

const FILE = '.hypothesisloop/experiment.yaml'

const listed = (words: string[]) => words.length === 1 ? words[0] : `${words.slice(0, -1).join(', ')} and ${words.at(-1)}`

const BRIEF_PLACEHOLDER = 'What to find out, the number that decides it and how it is measured, what to compare against, and what must stay the same.'

/** Step 1: the code, the name and the brief. */
export function StepWhat({ form, set, isEdit, locked, fileNote, onRead }: StepProps & {
  fileNote: FileNote | null
  /** Takes the repository and branch named and reads the experiment file there. */
  onRead: (repository: string, branch: string) => void
}) {
  const at = (note: FileNote) => `${note.repository} at ${note.branch}`
  return (
    <>
      {isEdit ? (
        <FieldGroup label="Repository and agent image" hint="Fixed when the experiment was created.">
          <Text>{form.repository ? `${form.repository} at ${form.branch}` : 'No repository'} · {form.agent_image || 'the platform\'s agent image'}</Text>
        </FieldGroup>
      ) : (
        <>
          <FormField label="Repository">
            <RepositorySelect value={form.repository} onChange={r => onRead(r, form.branch)} />
            {!fileNote && <Text variant="hint">A repository with {FILE} fills in the rest of this form.</Text>}
            {fileNote?.state === 'reading' && <Text variant="hint" role="status">Reading {FILE} of {at(fileNote)}…</Text>}
            {fileNote?.state === 'filled' && (
              <Text variant="note" role="status">
                {fileNote.filled.length > 0
                  ? `Filled in ${listed(fileNote.filled)} from ${FILE}.`
                  : `${FILE} of ${at(fileNote)} states nothing this form does not already hold.`}
              </Text>
            )}
            {fileNote?.state === 'none' && <Text variant="note" role="status">{at(fileNote)} has no {FILE}.</Text>}
            {fileNote?.state === 'elsewhere' && <Text variant="note" role="status">{fileNote.message}</Text>}
            {fileNote?.state === 'failed' && (
              <Inline gap="sm">
                <InlineError>Could not read {FILE} of {at(fileNote)}: {fileNote.message}</InlineError>
                <Button type="button" size="sm" onClick={() => onRead(fileNote.repository, fileNote.branch)}>Try again</Button>
              </Inline>
            )}
          </FormField>
          {namesRepository(form.repository) && (
            <FormField label="Branch" hint="The branch agents clone and start from. None of them pushes to it.">
              <Input
                mono
                value={form.branch}
                onChange={e => set('branch', e.target.value)}
                onBlur={() => { if (!fileNote) onRead(form.repository, form.branch) }}
              />
            </FormField>
          )}
        </>
      )}
      <FormField label="Name">
        <Input value={form.name} disabled={locked('name')} onChange={e => set('name', e.target.value)} placeholder="e.g. Q3 Vision Benchmark" />
      </FormField>
      <FormField
        label="Brief"
        hint="The only text every agent reads about the task."
      >
        <Textarea
          rows={8}
          className="overflow-x-hidden break-words"
          value={form.description}
          disabled={locked('description')}
          onChange={e => set('description', e.target.value)}
          placeholder={BRIEF_PLACEHOLDER}
        />
      </FormField>
      <BriefGuide />
    </>
  )
}

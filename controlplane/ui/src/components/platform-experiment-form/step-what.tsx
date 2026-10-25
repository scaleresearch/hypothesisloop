import { useGitHub } from '@/components/connect-github-step'
import { GoalGuide } from '@/components/goal-guide'
import { namesRepository, RepositorySelect } from '@/components/repository-select'
import { Button } from '@/components/ui/button'
import { FieldGroup, FormField } from '@/components/ui/form-field'
import { Input, Textarea } from '@/components/ui/input'
import { Inline } from '@/components/ui/stack'
import { InlineError } from '@/components/ui/status-message'
import { Text } from '@/components/ui/text'
import type { StepOneProblems, StepProps } from '@/components/platform-experiment-form/model'

/**
 * What the experiment file at the chosen repository and branch did to the form: is being read,
 * filled the fields named, is not there, is the template's not yet filled in, places the
 * experiment elsewhere, cannot be used, or could not be read. The API's message for one
 * elsewhere or unusable names the file and where.
 */
export type FileNote =
  | { repository: string; branch: string; state: 'reading' | 'none' | 'unfilled' }
  | { repository: string; branch: string; state: 'filled'; filled: string[] }
  | { repository: string; branch: string; state: 'elsewhere' | 'invalid' | 'failed'; message: string }

const FILE = '.hypothesisloop/experiment.yaml'

const listed = (words: string[]) => words.length === 1 ? words[0] : `${words.slice(0, -1).join(', ')} and ${words.at(-1)}`

const GOAL_PLACEHOLDER = 'What to find out, how it is measured, what to compare against, what stays the same.'

/** Step 1: the code, the name and the experiment goal. `problems` say what stops the create here, beside each field. */
export function StepWhat({ form, set, isEdit, locked, fileNote, onRead, problems = {} }: StepProps & {
  fileNote: FileNote | null
  /** Takes the repository and branch named and reads the experiment file there. */
  onRead: (repository: string, branch: string) => void
  problems?: StepOneProblems
}) {
  const github = useGitHub()
  const at = (note: FileNote) => `${note.repository} at ${note.branch}`
  return (
    <>
      {isEdit ? (
        <FieldGroup label="Repository and agent image" hint="Fixed when the experiment was created.">
          <Text>{form.repository ? `${form.repository} at ${form.branch}` : 'No repository'} · {form.agent_image || 'the platform\'s agent image'}</Text>
        </FieldGroup>
      ) : (
        <>
          <FormField label="Repository" error={problems.repository && <span id="repository-problem">{problems.repository}</span>}>
            <RepositorySelect value={form.repository} onChange={r => onRead(r, form.branch)} invalid={!!problems.repository} describedBy={problems.repository ? 'repository-problem' : undefined} />
            {github === 'connected' && !namesRepository(form.repository) && <Text variant="hint">A new repository starts from the experiment template.</Text>}
            {fileNote?.state === 'reading' && <Text variant="hint" role="status">Reading {FILE} of {at(fileNote)}…</Text>}
            {fileNote?.state === 'filled' && (
              <Text variant="note" role="status">
                {fileNote.filled.length > 0
                  ? `Filled in ${listed(fileNote.filled)} from ${FILE}.`
                  : `${FILE} of ${at(fileNote)} states nothing this form does not already hold.`}
              </Text>
            )}
            {fileNote?.state === 'none' && <Text variant="note" role="status">{at(fileNote)} has no {FILE}.</Text>}
            {fileNote?.state === 'unfilled' && <Text variant="note" role="status">{`${FILE} of ${at(fileNote)} is the template's, not yet filled in.`}</Text>}
            {fileNote?.state === 'elsewhere' && <Text variant="note" role="status">{fileNote.message}</Text>}
            {(fileNote?.state === 'invalid' || fileNote?.state === 'failed') && (
              <Inline gap="sm">
                <InlineError>{fileNote.state === 'invalid' ? fileNote.message : `Could not read ${FILE} of ${at(fileNote)}: ${fileNote.message}`}</InlineError>
                <Button type="button" size="sm" onClick={() => onRead(fileNote.repository, fileNote.branch)}>Try again</Button>
              </Inline>
            )}
          </FormField>
          {namesRepository(form.repository) && (
            <FormField label="Branch">
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
      <FormField label="Name" error={problems.name}>
        <Input value={form.name} invalid={!!problems.name} disabled={locked('name')} onChange={e => set('name', e.target.value)} placeholder="e.g. Q3 Vision Benchmark" />
      </FormField>
      <FormField label="Experiment goal" error={problems.description}>
        <Textarea
          rows={8}
          invalid={!!problems.description}
          className="overflow-x-hidden break-words"
          value={form.description}
          disabled={locked('description')}
          onChange={e => set('description', e.target.value)}
          placeholder={GOAL_PLACEHOLDER}
        />
      </FormField>
      <GoalGuide />
    </>
  )
}

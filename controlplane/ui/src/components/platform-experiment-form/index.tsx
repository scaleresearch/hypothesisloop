'use client'

import { forwardRef, useEffect, useImperativeHandle, useRef, useState } from 'react'
import { useSWRConfig } from 'swr'
import {
  ApiError, NO_EXPERIMENT_FILE, createExperimentDraft, createPlatformExperiment, deleteExperimentDraft, fetchExperimentFile, updateExperimentDraft, updatePlatformExperiment,
} from '@/lib/api'
import type { CreatePlatformExperimentRequest, ExperimentDraft, PlatformExperimentPatch } from '@/lib/api'
import { isZeroDate } from '@/lib/format'
import { useTenant } from '@/lib/tenant'
import { PlatformExperimentStatus } from '@/types'
import type { PlatformExperiment } from '@/types'
import { repositoryFields } from '@/components/repository-select'
import { ConnectGitHubStep, useNeedsGitHub } from '@/components/connect-github-step'
import { Button } from '@/components/ui/button'
import { Dialog } from '@/components/ui/dialog'
import { Inline, Stack } from '@/components/ui/stack'
import { ErrorMessage } from '@/components/ui/status-message'
import { Tabs } from '@/components/ui/tabs'
import { Text } from '@/components/ui/text'
import { NEW_EXPERIMENT, STEPS, refusedStep, toDrafts, toResearchers, toStages } from '@/components/platform-experiment-form/model'
import type { FormState, Problem, StepID } from '@/components/platform-experiment-form/model'
import { StepWhat } from '@/components/platform-experiment-form/step-what'
import type { FileNote } from '@/components/platform-experiment-form/step-what'
import { StepMeasure } from '@/components/platform-experiment-form/step-measure'
import { StepBudget } from '@/components/platform-experiment-form/step-budget'
import { errorMessage } from '@/lib/errors'

interface FormProps {
  /** The experiment to edit. Without one the form creates one in the selected project. */
  initial?: PlatformExperiment | null
  /** The draft a new experiment is finished from, at the step it was left on. */
  draft?: ExperimentDraft
  /** Told the experiment the API answered with, once it is saved. `created` is set for a new one. */
  onSaved: (saved: PlatformExperiment, created?: boolean) => void
  /** Offers Cancel. A form that is the page's own content has nothing to go back to and passes none. */
  onCancel?: () => void
}

/** What the dialog asks of the form it holds: to be left, keeping what was typed. */
export interface FormHandle {
  leave: () => void
}

// The fields the repository's experiment file can fill, by the words the form names them with.
const FILE_FIELDS = [['name', 'the name'], ['description', 'the brief'], ['metrics', 'the metrics'], ['report_interval_seconds', 'the report interval'], ['stages', 'the stage ladder']] as const

/**
 * The one form a platform experiment is created and edited with, in three steps. A new one is
 * run by the platform: one create stands up a hosted coordinator and the starting agents. Every
 * rule is the API's: the editors hint at what the API will refuse as it is typed, and a refusal
 * is shown at the step it concerns. A new experiment left with anything typed is kept as a draft.
 */
export const PlatformExperimentForm = forwardRef<FormHandle, FormProps>(function PlatformExperimentForm({ initial, draft, onSaved, onCancel }, ref) {
  const { project } = useTenant()
  const { mutate } = useSWRConfig()
  const needsGitHub = useNeedsGitHub()
  const isEdit = !!initial
  // Once running, admission and the stage ladder have acted on everything but the name, the
  // description, the duration and the researchers, and the API takes no other field, so the rest is
  // locked here. The researchers are changed in place on the experiment's page.
  const lockedWhileRunning = isEdit && initial?.status === PlatformExperimentStatus.RUNNING
  const [form, setForm] = useState<FormState>(() => ({
    name: initial?.name ?? '',
    description: initial?.description ?? '',
    budget_accelerator_hours: String(initial?.budget_accelerator_hours ?? NEW_EXPERIMENT.budget_accelerator_hours),
    max_agents: String(initial?.max_agents ?? NEW_EXPERIMENT.researchers),
    starting_agents: null,
    max_concurrent_accelerators: initial?.max_concurrent_accelerators != null ? String(initial.max_concurrent_accelerators) : '',
    starts_at: !isZeroDate(initial?.starts_at) ? new Date(initial!.starts_at!).toISOString().slice(0, 16) : '',
    duration_hours: String(initial?.duration_hours ?? NEW_EXPERIMENT.duration_hours),
    metrics: initial?.metrics ?? [],
    report_interval_seconds: initial?.report_interval_seconds ? String(initial.report_interval_seconds) : '',
    stages: initial?.stages?.length ? toDrafts(initial.stages) : null,
    repository: initial?.repository ?? '',
    agent_image: initial?.agent_image ?? '',
    ...draft?.form as Partial<FormState> | undefined,
  }))
  // The form as it opened. An edit sends the fields that differ from it, and a new one left
  // unchanged needs no draft kept.
  const [opened] = useState(form)
  const [kept, setKept] = useState(form)
  const [step, setStep] = useState<StepID>(() => STEPS.find(s => s.id === draft?.step)?.id ?? '1')
  const [fromFile, setFromFile] = useState<FileNote | null>(null)
  const [submitting, setSubmitting] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [draftID, setDraftID] = useState(draft?.id ?? null)
  const [savingDraft, setSavingDraft] = useState(false)
  // Why the last create was refused, kept until the next try: each problem the API named, or the
  // one sentence a draft kept of them.
  const [refused, setRefused] = useState<Problem[] | null>(draft?.failure ? [{ message: draft.failure }] : null)
  const failure = refused?.map(p => p.message).join(' ') ?? ''
  const panel = useRef<HTMLDivElement>(null)
  const moved = useRef(false)

  const unsaved = JSON.stringify(form) !== JSON.stringify(kept)

  function set<K extends keyof FormState>(field: K, value: FormState[K]) {
    setForm(f => ({ ...f, [field]: value }))
  }

  // A step that changed takes the focus to its first field, so the keyboard goes on from there.
  useEffect(() => {
    if (!moved.current) return
    panel.current?.querySelector<HTMLElement>('input:not([disabled]), textarea:not([disabled]), select:not([disabled]), button:not([disabled])')?.focus()
  }, [step])

  // Keeps the form as it stands as this form's draft, the one it opened from or a new one.
  async function keepDraft(at: StepID, failure: string): Promise<void> {
    const body = { form: { ...form }, step: at, failure }
    const saved = draftID ? await updateExperimentDraft(project.id, draftID, body) : await createExperimentDraft(project.id, body)
    setDraftID(saved.id)
    setKept(body.form)
    void mutate(['experiment-drafts', project.id])
  }

  async function saveDraft() {
    setSavingDraft(true)
    setError(null)
    try {
      await keepDraft(step, failure)
    } catch (err: unknown) {
      setError(`Could not save the draft: ${errorMessage(err)}`)
    } finally {
      setSavingDraft(false)
    }
  }

  // Leaving a new experiment with something typed since it was last kept keeps it first. One that
  // cannot be kept stays open, with why, so that nothing typed is lost.
  async function leave() {
    if (isEdit || !unsaved) return onCancel?.()
    setSavingDraft(true)
    try {
      await keepDraft(step, failure)
      settled.current = true
      onCancel?.()
    } catch (err: unknown) {
      setError(`Could not keep what you typed as a draft, so the form stays open: ${errorMessage(err)}`)
      setSavingDraft(false)
    }
  }

  useImperativeHandle(ref, () => ({ leave: () => void leave() }))

  // A form taken off the page some other way, by the browser's Back or a link, keeps what was
  // typed all the same, and a reload or a closed tab asks first.
  const settled = useRef(false)
  const keepOnUnmount = useRef<() => void>(() => undefined)
  keepOnUnmount.current = () => {
    if (!isEdit && unsaved && !settled.current) void keepDraft(step, failure)
  }
  useEffect(() => () => keepOnUnmount.current(), [])
  useEffect(() => {
    if (isEdit || !unsaved) return
    const ask = (e: BeforeUnloadEvent) => e.preventDefault()
    window.addEventListener('beforeunload', ask)
    return () => window.removeEventListener('beforeunload', ask)
  }, [isEdit, unsaved])

  function go(next: StepID) {
    setError(null)
    moved.current = true
    setStep(next)
  }

  // A repository may bring its own experiment: the brief, the metrics and the ladder its code
  // is written for. Choosing it fills them in, for the person to read and change before anything
  // is created. A name or a brief already typed stays.
  async function chooseRepository(repository: string) {
    set('repository', repository)
    if (!repository) return setFromFile(null)
    setFromFile({ repository, state: 'reading' })
    try {
      const file = await fetchExperimentFile(project.id, repository)
      const next = (f: FormState): FormState => ({
        ...f,
        name: f.name.trim() || file.name || '',
        description: f.description.trim() || file.description || '',
        metrics: file.metrics?.length ? file.metrics : f.metrics,
        report_interval_seconds: file.report_interval_seconds ? String(file.report_interval_seconds) : f.report_interval_seconds,
        stages: file.stages?.length ? toDrafts(file.stages) : f.stages,
      })
      const filled = FILE_FIELDS.filter(([field]) => JSON.stringify(next(form)[field]) !== JSON.stringify(form[field])).map(([, words]) => words)
      setForm(f => f.repository !== repository ? f : next(f))
      setFromFile({ repository, state: 'filled', filled })
    } catch (err: unknown) {
      if (err instanceof ApiError && err.reason === NO_EXPERIMENT_FILE) return setFromFile({ repository, state: 'none' })
      setFromFile({ repository, state: 'failed', message: errorMessage(err) })
    }
  }

  // What an edit changed, as the fields of the update. A date is compared as the form holds it,
  // which is to the minute, so one left alone is never sent back rounded.
  function changes(): PlatformExperimentPatch {
    const patch: PlatformExperimentPatch = {}
    if (form.name.trim() !== opened.name) patch.name = form.name.trim()
    if (form.description.trim() !== opened.description) patch.description = form.description.trim()
    if (Number(form.duration_hours) !== Number(opened.duration_hours)) patch.duration_hours = Number(form.duration_hours)
    if (lockedWhileRunning) return patch
    if (Number(form.budget_accelerator_hours) !== Number(opened.budget_accelerator_hours)) patch.budget_accelerator_hours = Number(form.budget_accelerator_hours)
    if (form.max_concurrent_accelerators !== opened.max_concurrent_accelerators) patch.max_concurrent_accelerators = Number(form.max_concurrent_accelerators)
    if (JSON.stringify(form.metrics) !== JSON.stringify(opened.metrics)) patch.metrics = form.metrics
    if (form.report_interval_seconds !== opened.report_interval_seconds) patch.report_interval_seconds = Number(form.report_interval_seconds)
    if (form.starts_at !== opened.starts_at) patch.starts_at = new Date(form.starts_at).toISOString()
    return patch
  }

  // The update takes a value for each of these and nothing that puts one back to unset, so an
  // edit that empties one that was set cannot be sent at all.
  function clearedError(): string | null {
    if (!isEdit) return null
    const cleared = ([
      ['starts_at', 'Starts at'],
      ['max_concurrent_accelerators', 'Max concurrent accelerators'],
      ['report_interval_seconds', 'Report interval'],
    ] as const).filter(([field]) => opened[field].trim() !== '' && form[field].trim() === '').map(([, label]) => label)
    return cleared.length > 0 ? `${cleared.join(', ')} cannot be emptied once set: state a value.` : null
  }

  async function handleSubmit(e: React.FormEvent) {
    e.preventDefault()
    const err = clearedError()
    if (err) { setError(err); return }
    setSubmitting(true)
    setError(null)
    try {
      if (isEdit && initial) {
        const patch = changes()
        onSaved(Object.keys(patch).length > 0 ? await updatePlatformExperiment(initial.id, patch) : initial)
        return
      }
      const payload: CreatePlatformExperimentRequest = {
        name: form.name.trim(),
        description: form.description.trim(),
        budget_accelerator_hours: Number(form.budget_accelerator_hours),
        max_agents: Number(form.max_agents),
        researchers: toResearchers(form.starting_agents, Number(form.max_agents)),
        ...(form.max_concurrent_accelerators.trim() !== '' ? { max_concurrent_accelerators: Number(form.max_concurrent_accelerators) } : {}),
        metrics: form.metrics,
        ...(form.report_interval_seconds.trim() !== '' ? { report_interval_seconds: Number(form.report_interval_seconds) } : {}),
        ...(form.stages ? { stages: toStages(form.stages) } : {}),
        ...repositoryFields(form.repository),
        ...(form.agent_image.trim() ? { agent_image: form.agent_image.trim() } : {}),
        duration_hours: Number(form.duration_hours),
      }
      setRefused(null)
      // No start time: the platform starts it once its baseline is declared.
      const experiment = await createPlatformExperiment(project.id, { ...payload, coordinator_hosted: true })
      // The draft goes once the experiment it was for exists. One that stays is listed, to delete.
      if (draftID) await deleteExperimentDraft(project.id, draftID).catch(() => undefined)
      void mutate(['experiment-drafts', project.id])
      settled.current = true
      onSaved(experiment, true)
    } catch (err: unknown) {
      if (isEdit) {
        setError(errorMessage(err))
        return
      }
      // A refused create keeps what was typed, as a draft that names why, at the step of its first problem.
      const problems = problemsOf(err)
      const at = refusedStep(problems[0].reason)
      setRefused(problems)
      go(at)
      try {
        await keepDraft(at, problems.map(p => p.message).join(' '))
      } catch (unkept: unknown) {
        setError(`Could not save the draft: ${errorMessage(unkept)}`)
      }
    } finally {
      setSubmitting(false)
    }
  }

  const at = STEPS.findIndex(s => s.id === step)
  const next = STEPS[at + 1]
  const back = STEPS[at - 1]
  const fileNote = fromFile?.repository === form.repository ? fromFile : null
  const stepProps = { form, set, isEdit, locked: lockedWhileRunning }

  return (
    <Stack gap="xl">
      <Tabs label="Steps" tabs={[...STEPS]} value={step} onChange={go} />

      <Stack as="form" gap="xl" onSubmit={handleSubmit}>
        <div ref={panel} role="tabpanel" aria-label={STEPS[at].label} className="grid gap-6">
          {step === '1' && <StepWhat {...stepProps} fileNote={fileNote} onRepository={r => void chooseRepository(r)} />}
          {step === '2' && <StepMeasure {...stepProps} />}
          {step === '3' && <StepBudget {...stepProps} />}
        </div>

        {refused && (
          <Stack gap="xs">
            <ErrorMessage>
              {refused.length === 1 ? refused[0].message : (
                <ul className="grid list-disc gap-1 pl-4">
                  {refused.map((p, i) => <li key={i}>{p.message}</li>)}
                </ul>
              )}
            </ErrorMessage>
            <Text variant="hint">Not created. {draftID ? 'What you typed is kept as a draft: change what the refusal names and try again.' : 'Change what the refusal names and try again.'}</Text>
          </Stack>
        )}
        {error && <ErrorMessage>{error}</ErrorMessage>}

        <Inline nowrap className="mt-1">
          {onCancel && <Button type="button" disabled={savingDraft} onClick={() => void leave()}>Cancel</Button>}
          {!isEdit && <Button type="button" loading={savingDraft} disabled={!unsaved} onClick={() => void saveDraft()}>{draftID && !unsaved ? 'Draft saved' : 'Save draft'}</Button>}
          <span className="flex-1" />
          {back && <Button type="button" onClick={() => go(back.id)}>Back</Button>}
          {next && <Button type="button" variant={isEdit || refused ? 'default' : 'primary'} onClick={() => go(next.id)}>Next: {next.label.replace(/^\d+\. /, '')}</Button>}
          {(isEdit || !next || refused) && (!isEdit && needsGitHub ? <ConnectGitHubStep /> : (
            <Button type="submit" variant="primary" loading={submitting}>
              {isEdit ? 'Save changes' : refused ? 'Try again' : 'Create experiment'}
            </Button>
          ))}
        </Inline>
      </Stack>
    </Stack>
  )
})

// Every problem a refusal names: the several of a check that found more than one, or the one.
function problemsOf(error: unknown): Problem[] {
  if (!(error instanceof ApiError)) return [{ message: errorMessage(error) }]
  const listed = error.body?.problems
  if (Array.isArray(listed) && listed.length > 0) return listed as Problem[]
  return [{ reason: error.reason, message: error.message }]
}

/** The form in a dialog, which closes once the experiment is saved. Escape and a click outside leave it as Cancel does. */
export function PlatformExperimentDialog({ initial, draft, onSaved, onClose }: {
  initial?: PlatformExperiment | null
  draft?: ExperimentDraft
  onSaved: FormProps['onSaved']
  onClose: () => void
}) {
  const form = useRef<FormHandle>(null)
  return (
    <Dialog open onClose={() => form.current?.leave()} title={initial ? 'Edit experiment' : 'New experiment'}>
      <PlatformExperimentForm ref={form} initial={initial} draft={draft} onCancel={onClose} onSaved={(saved, created) => { onSaved(saved, created); onClose() }} />
    </Dialog>
  )
}

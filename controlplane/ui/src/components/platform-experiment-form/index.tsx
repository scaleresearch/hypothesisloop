'use client'

import { forwardRef, useEffect, useImperativeHandle, useRef, useState } from 'react'
import { useSWRConfig } from 'swr'
import { createExperimentDraft, deleteExperimentDraft, updateExperimentDraft } from '@/lib/api/drafts'
import { createPlatformExperiment, fetchExperimentFile, NO_EXPERIMENT_FILE, updatePlatformExperiment } from '@/lib/api/platform-experiments'
import type { CreatePlatformExperimentRequest, PlatformExperimentPatch } from '@/types/api'
import { ApiError } from '@/lib/api/request'
import { isZeroDate } from '@/lib/format'
import { useTenant } from '@/lib/tenant'
import { repositoryFields } from '@/components/repository-select'
import { ConnectGitHubStep, useNeedsGitHub } from '@/components/connect-github-step'
import { Button } from '@/components/ui/button'
import { Dialog } from '@/components/ui/dialog'
import { Inline, Stack } from '@/components/ui/stack'
import { ErrorMessage } from '@/components/ui/status-message'
import { Tabs } from '@/components/ui/tabs'
import { Text } from '@/components/ui/text'
import { NEW_EXPERIMENT, STEPS, refusedStep, toDrafts, toResearchers, toStages, toStartingAgents } from '@/components/platform-experiment-form/model'
import type { FormState, Problem, StepID } from '@/components/platform-experiment-form/model'
import { StepWhat } from '@/components/platform-experiment-form/step-what'
import type { FileNote } from '@/components/platform-experiment-form/step-what'
import { StepMeasure } from '@/components/platform-experiment-form/step-measure'
import { StepBudget } from '@/components/platform-experiment-form/step-budget'
import { errorMessage } from '@/lib/errors'
import { keys } from '@/lib/keys'
import type { ExperimentDraft, PlatformExperiment } from '@/types/api'
import { canEdit, type EditableField } from '@/lib/editable'

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
interface FormHandle {
  leave: () => void
}

// Every field of the form the repository's experiment file fills, by the words the form names it with.
const FILE_FIELDS = [
  ['name', 'the name'], ['description', 'the brief'], ['metrics', 'the metrics'], ['budget_accelerator_hours', 'the budget'],
  ['max_agents', 'the max researchers'], ['duration_hours', 'the duration'], ['starting_agents', 'the starting agents'],
  ['report_interval_seconds', 'the report interval'], ['stages', 'the stage ladder'], ['max_concurrent_accelerators', 'the max concurrent accelerators'],
  ['agent_image', 'the agent image'], ['coordinator_model', 'the coordinator model'], ['default_quota_tier', 'the default quota tier'],
] as const

/**
 * The one form a platform experiment is created and edited with, in three steps. A new one is
 * run by the platform: one create stands up a sandboxed coordinator and the starting agents. Every
 * rule is the API's: the editors hint at what the API will refuse as it is typed, and a refusal
 * is shown at the step it concerns. A new experiment left with anything typed is kept as a draft.
 */
export const PlatformExperimentForm = forwardRef<FormHandle, FormProps>(function PlatformExperimentForm({ initial, draft, onSaved, onCancel }, ref) {
  const { project } = useTenant()
  const { mutate } = useSWRConfig()
  const needsGitHub = useNeedsGitHub()
  const isEdit = !!initial
  // An edit changes the fields the API lists in editable and locks the rest. The researchers are
  // changed in place on the experiment's page.
  const locked = (field: EditableField) => !!initial && !canEdit(initial, field)
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
    coordinator_model: initial?.coordinator?.model ?? '',
    default_quota_tier: initial?.default_quota_tier ?? '',
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
    void mutate(keys.experimentDrafts(project.id))
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

  // A repository may bring its own experiment spec, the one hl platform-experiments start reads.
  // Choosing it fills in every field it states, for the person to read and change before the
  // whole body is sent. A name or a brief already typed stays.
  async function chooseRepository(repository: string) {
    set('repository', repository)
    if (!repository) return setFromFile(null)
    setFromFile({ repository, state: 'reading' })
    try {
      const file = await fetchExperimentFile(project.id, repository)
      const stated = (value: number | string | undefined, held: string) => value ? String(value) : held
      const next = (f: FormState): FormState => ({
        ...f,
        name: f.name.trim() || file.name || '',
        description: f.description.trim() || file.description || '',
        metrics: file.metrics?.length ? file.metrics : f.metrics,
        budget_accelerator_hours: stated(file.budget_accelerator_hours, f.budget_accelerator_hours),
        max_agents: stated(file.max_agents, f.max_agents),
        duration_hours: stated(file.duration_hours, f.duration_hours),
        starting_agents: file.researchers?.length ? toStartingAgents(file.researchers) : f.starting_agents,
        report_interval_seconds: stated(file.report_interval_seconds, f.report_interval_seconds),
        stages: file.stages?.length ? toDrafts(file.stages) : f.stages,
        max_concurrent_accelerators: stated(file.max_concurrent_accelerators, f.max_concurrent_accelerators),
        agent_image: stated(file.agent_image, f.agent_image),
        coordinator_model: stated(file.coordinator_model, f.coordinator_model),
        default_quota_tier: stated(file.default_quota_tier, f.default_quota_tier),
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
    if (!locked('name') && form.name.trim() !== opened.name) patch.name = form.name.trim()
    if (!locked('description') && form.description.trim() !== opened.description) patch.description = form.description.trim()
    if (!locked('duration_hours') && Number(form.duration_hours) !== Number(opened.duration_hours)) patch.duration_hours = Number(form.duration_hours)
    if (!locked('budget_accelerator_hours') && Number(form.budget_accelerator_hours) !== Number(opened.budget_accelerator_hours)) patch.budget_accelerator_hours = Number(form.budget_accelerator_hours)
    if (!locked('max_concurrent_accelerators') && form.max_concurrent_accelerators !== opened.max_concurrent_accelerators) patch.max_concurrent_accelerators = Number(form.max_concurrent_accelerators)
    if (!locked('metrics') && JSON.stringify(form.metrics) !== JSON.stringify(opened.metrics)) patch.metrics = form.metrics
    if (!locked('report_interval_seconds') && form.report_interval_seconds !== opened.report_interval_seconds) patch.report_interval_seconds = Number(form.report_interval_seconds)
    if (!locked('starts_at') && form.starts_at !== opened.starts_at) patch.starts_at = new Date(form.starts_at).toISOString()
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
        ...(form.coordinator_model.trim() ? { coordinator_model: form.coordinator_model.trim() } : {}),
        ...(form.default_quota_tier ? { default_quota_tier: form.default_quota_tier } : {}),
        duration_hours: Number(form.duration_hours),
      }
      setRefused(null)
      // No start time: the platform starts it once its baseline is declared.
      const experiment = await createPlatformExperiment(project.id, { ...payload, coordinator_in_agentsandbox: true })
      // The draft goes once the experiment it was for exists. One that stays is listed, to delete.
      if (draftID) await deleteExperimentDraft(project.id, draftID).catch(() => undefined)
      void mutate(keys.experimentDrafts(project.id))
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
  const stepProps = { form, set, isEdit, locked }

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
            <Text variant="hint">Not created. Change what the refusal names and try again.</Text>
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
  if (listed && listed.length > 0) return listed
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

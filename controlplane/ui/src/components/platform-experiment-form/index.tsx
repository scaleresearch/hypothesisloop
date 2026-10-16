'use client'

import { useState } from 'react'
import { useSWRConfig } from 'swr'
import {
  ApiError, NO_EXPERIMENT_FILE, createExperimentDraft, deleteExperimentDraft, fetchExperimentFile, updateExperimentDraft, updatePlatformExperiment,
} from '@/lib/api'
import type { CreatePlatformExperimentRequest, ExperimentDraft, PlatformExperimentPatch } from '@/lib/api'
import type { TypeKind } from '@/lib/units'
import { isZeroDate } from '@/lib/format'
import { createAndRun } from '@/lib/run-experiment'
import type { RunFailure } from '@/lib/run-experiment'
import { projectPath, withQuery } from '@/lib/routes'
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
import { NEW_EXPERIMENT, STEPS, refusedStep, stagesError, toDrafts, toStages } from '@/components/platform-experiment-form/model'
import type { FormState, StepID } from '@/components/platform-experiment-form/model'
import { StepWhat } from '@/components/platform-experiment-form/step-what'
import type { FileNote } from '@/components/platform-experiment-form/step-what'
import { StepMeasure } from '@/components/platform-experiment-form/step-measure'
import { StepBudget } from '@/components/platform-experiment-form/step-budget'

interface FormProps {
  /** The experiment to edit. Without one the form creates one in the selected project. */
  initial?: PlatformExperiment | null
  /** The draft a new experiment is finished from, at the step it was left on. */
  draft?: ExperimentDraft
  /** What the budget of the experiment being edited is spent on, when the page knows. */
  budgetKind?: TypeKind
  /**
   * Told the experiment the API answered with, once it is saved. `run` is set when the form went
   * on to add hosted agents, with what stopped that when something did.
   */
  onSaved: (saved: PlatformExperiment, run?: { failure?: RunFailure }) => void
  /** Offers Cancel. A form that is the page's own content has nothing to go back to and passes none. */
  onCancel?: () => void
}

/**
 * The one form a platform experiment is created and edited with, in three steps. A new one is
 * run by the platform: a hosted coordinator and the researchers asked for. It checks only what
 * it needs to build the request; every other rule is the API's, whose refusal it shows.
 */
export function PlatformExperimentForm({ initial, draft, budgetKind, onSaved, onCancel }: FormProps) {
  const { project } = useTenant()
  const { mutate } = useSWRConfig()
  const needsGitHub = useNeedsGitHub()
  const isEdit = !!initial
  // Once running, admission and the stage ladder have acted on everything but the name, the
  // description and the duration, and the API takes no other field, so the rest is locked here.
  const lockedWhileRunning = isEdit && initial?.status === PlatformExperimentStatus.RUNNING
  const [form, setForm] = useState<FormState>(() => ({
    name: initial?.name ?? '',
    description: initial?.description ?? '',
    budget_accelerator_hours: String(initial?.budget_accelerator_hours ?? NEW_EXPERIMENT.budget_accelerator_hours),
    max_agents: String(initial?.max_agents ?? NEW_EXPERIMENT.researchers),
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
  // The form as it opened. An edit sends the fields that differ from it.
  const [opened] = useState(form)
  const [step, setStep] = useState<StepID>(() => STEPS.find(s => s.id === draft?.step)?.id ?? '1')
  const [fromFile, setFromFile] = useState<FileNote | null>(null)
  const [submitting, setSubmitting] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [draftID, setDraftID] = useState(draft?.id ?? null)
  const [savingDraft, setSavingDraft] = useState(false)
  const [draftSaved, setDraftSaved] = useState(false)
  // Why the last create was refused, kept until the next try.
  const [refused, setRefused] = useState(draft?.failure || null)
  // What the API said of a new repository GitHub would not give it, shown where one is chosen.

  function set<K extends keyof FormState>(field: K, value: FormState[K]) {
    setDraftSaved(false)
    setForm(f => ({ ...f, [field]: value }))
  }

  // Keeps the form as it stands as this form's draft, the one it opened from or a new one.
  async function keepDraft(at: StepID, failure: string): Promise<string> {
    const body = { form: { ...form }, step: at, failure }
    const kept = draftID ? await updateExperimentDraft(project.id, draftID, body) : await createExperimentDraft(project.id, body)
    setDraftID(kept.id)
    void mutate(['experiment-drafts', project.id])
    return kept.id
  }

  async function saveDraft() {
    setSavingDraft(true)
    setError(null)
    try {
      await keepDraft(step, refused ?? '')
      setDraftSaved(true)
    } catch (err: unknown) {
      setError(`Could not save the draft: ${said(err)}`)
    } finally {
      setSavingDraft(false)
    }
  }

  // Leaving for GitHub keeps the form as a draft, which GitHub's return opens again.
  async function leaveWithDraft(): Promise<string> {
    return withQuery(projectPath(project.id, 'experiments'), { draft: await keepDraft(step, refused ?? '') })
  }

  function go(next: StepID) {
    setError(null)
    setStep(next)
  }

  // A repository may bring its own experiment: the brief, the metrics and the ladder its code
  // is written for. Choosing it fills them in, for the person to read and change before anything
  // is created. A name or a description already typed stays.
  async function chooseRepository(repository: string) {
    set('repository', repository)
    if (!repository) return setFromFile(null)
    setFromFile({ repository, state: 'reading' })
    try {
      const file = await fetchExperimentFile(project.id, repository)
      setForm(f => f.repository !== repository ? f : {
        ...f,
        name: f.name.trim() || file.name || '',
        description: f.description.trim() || file.description || '',
        metrics: file.metrics?.length ? file.metrics : f.metrics,
        report_interval_seconds: file.report_interval_seconds ? String(file.report_interval_seconds) : f.report_interval_seconds,
        stages: file.stages?.length ? toDrafts(file.stages) : f.stages,
      })
      setFromFile({ repository, state: 'filled' })
    } catch (err: unknown) {
      if (err instanceof ApiError && err.reason === NO_EXPERIMENT_FILE) return setFromFile({ repository, state: 'none' })
      setFromFile({ repository, state: 'failed', message: err instanceof Error ? err.message : String(err) })
    }
  }

  const ladderError = form.stages && !isEdit ? stagesError(form.stages) : null

  function formError(): string | null {
    if (!form.name.trim()) return 'Name is required.'
    if (!isEdit && !form.description.trim()) return 'A brief is required: it is the only text every agent reads about the task.'
    if (Number(form.duration_hours) <= 0) return 'Duration must be greater than 0.'
    if (Number(form.budget_accelerator_hours) <= 0) return 'Budget must be greater than 0.'
    if (!isEdit && Number(form.max_agents) < 1) return 'At least one researcher.'
    if (form.metrics.some(m => m.role === 'constraint' && m.bound === undefined)) return 'Every gate needs a bound.'
    return null
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
    if (Number(form.max_agents) !== Number(opened.max_agents)) patch.max_agents = Number(form.max_agents)
    if (form.max_concurrent_accelerators !== opened.max_concurrent_accelerators) patch.max_concurrent_accelerators = Number(form.max_concurrent_accelerators)
    if (JSON.stringify(form.metrics) !== JSON.stringify(opened.metrics)) patch.metrics = form.metrics
    if (form.report_interval_seconds !== opened.report_interval_seconds) patch.report_interval_seconds = Number(form.report_interval_seconds)
    if (form.starts_at !== opened.starts_at) patch.starts_at = new Date(form.starts_at).toISOString()
    return patch
  }

  // The update takes a value for each of these and nothing that puts one back to unset, so an
  // edit that empties one that was set is refused here.
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
    // Every step is checked here, whichever is showing: the steps themselves are never gated.
    const err = formError() ?? clearedError() ?? ladderError
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
        ...(form.max_concurrent_accelerators.trim() !== '' ? { max_concurrent_accelerators: Number(form.max_concurrent_accelerators) } : {}),
        metrics: form.metrics,
        ...(form.report_interval_seconds.trim() !== '' ? { report_interval_seconds: Number(form.report_interval_seconds) } : {}),
        ...(form.stages ? { stages: toStages(form.stages) } : {}),
        ...repositoryFields(form.repository),
        ...(form.agent_image.trim() ? { agent_image: form.agent_image.trim() } : {}),
        duration_hours: Number(form.duration_hours),
      }
      setRefused(null)
      const { experiment, failure } = await createAndRun(project.id, payload, Number(form.max_agents))
      // The draft goes once the experiment it was for exists.
      const dropped = draftID ? await deleteExperimentDraft(project.id, draftID).then(() => undefined, (e: unknown) => ({ step: 'deleting the draft it was made from', message: said(e) })) : undefined
      void mutate(['experiment-drafts', project.id])
      onSaved(experiment, { failure: failure ?? dropped })
    } catch (err: unknown) {
      if (isEdit) {
        setError(said(err))
        return
      }
      // A refused create keeps what was typed, as a draft that names why, at the step it concerns.
      const message = said(err)
      const at = refusedStep(err instanceof ApiError ? err.reason : undefined)
      setRefused(message)
      setStep(at)
      try {
        await keepDraft(at, message)
      } catch (unkept: unknown) {
        setError(`Could not save the draft: ${said(unkept)}`)
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
        {step === '1' && (
          <StepWhat {...stepProps} fileNote={fileNote} onRepository={r => void chooseRepository(r)} leave={leaveWithDraft} />
        )}
        {step === '2' && <StepMeasure {...stepProps} />}
        {step === '3' && <StepBudget {...stepProps} budgetKind={budgetKind} />}

        {refused && (
          <Stack gap="xs">
            <ErrorMessage>{refused}</ErrorMessage>
            <Text variant="hint">Not created. {draftID ? 'What you typed is kept as a draft: change what the refusal names and try again.' : 'Change what the refusal names and try again.'}</Text>
          </Stack>
        )}
        {error && <ErrorMessage>{error}</ErrorMessage>}

        <Inline nowrap className="mt-1">
          {onCancel && <Button type="button" onClick={onCancel}>Cancel</Button>}
          {!isEdit && <Button type="button" loading={savingDraft} onClick={() => void saveDraft()}>Save draft</Button>}
          {draftSaved && <Text as="span" variant="hint">Draft saved.</Text>}
          <span className="flex-1" />
          {back && <Button type="button" onClick={() => go(back.id)}>Back</Button>}
          {next && <Button type="button" variant={isEdit || refused ? 'default' : 'primary'} onClick={() => go(next.id)}>Next</Button>}
          {(isEdit || !next || refused) && (!isEdit && needsGitHub ? <ConnectGitHubStep /> : (
            <Button type="submit" variant="primary" loading={submitting} disabled={!!ladderError}>
              {isEdit ? 'Save changes' : refused ? 'Try again' : 'Create experiment'}
            </Button>
          ))}
        </Inline>
      </Stack>
    </Stack>
  )
}

function said(error: unknown): string {
  return error instanceof Error ? error.message : String(error)
}

/** The form in a dialog, which closes once the experiment is saved. */
export function PlatformExperimentDialog({ initial, draft, onSaved, onClose }: {
  initial?: PlatformExperiment | null
  draft?: ExperimentDraft
  onSaved: FormProps['onSaved']
  onClose: () => void
}) {
  return (
    <Dialog open onClose={onClose} title={initial ? 'Edit experiment' : 'New experiment'}>
      <PlatformExperimentForm initial={initial} draft={draft} onCancel={onClose} onSaved={(saved, run) => { onSaved(saved, run); onClose() }} />
    </Dialog>
  )
}

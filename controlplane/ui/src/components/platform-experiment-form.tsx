'use client'

import React, { useState } from 'react'
import { clsx } from 'clsx'
import { Combobox, ComboboxInput, ComboboxOption, ComboboxOptions } from '@headlessui/react'
import { Info } from 'lucide-react'
import { ApiError, NO_EXPERIMENT_FILE, fetchExperimentFile, repositoryNotProvided, updatePlatformExperiment } from '@/lib/api'
import type { CreatePlatformExperimentRequest, PlatformExperimentPatch } from '@/lib/api'
import { budgetUnit } from '@/lib/units'
import type { TypeKind } from '@/lib/units'
import type { Stage } from '@/lib/api'
import { isZeroDate } from '@/lib/format'
import { createAndRun } from '@/lib/run-experiment'
import type { RunFailure } from '@/lib/run-experiment'
import { useTenant } from '@/lib/tenant'
import { COMMON_ML_METRICS, PlatformExperimentStatus } from '@/types'
import type { MetricDefinition, PlatformExperiment } from '@/types'
import { RepositorySelect, repositoryFields } from '@/components/repository-select'
import { ConnectGitHubStep, useNeedsGitHub } from '@/components/connect-github-step'
import { Button } from '@/components/ui/button'
import { Dialog } from '@/components/ui/dialog'
import { Disclosure } from '@/components/ui/disclosure'
import { fieldClass } from '@/components/ui/field'
import { FieldGroup, FormField } from '@/components/ui/form-field'
import { Input, Textarea } from '@/components/ui/input'
import { OptionLabel, optionClass, panelClass } from '@/components/ui/option'
import { Segmented, Tabs } from '@/components/ui/tabs'
import { Eyebrow, Pill } from '@/components/ui/text'
import { ErrorMessage, InlineError } from '@/components/ui/status-message'

// What a new experiment starts from. The stage ladder and the report interval are left out, so
// the platform applies its own (stages.default, scheduler.default_report_interval_seconds).
const NEW_EXPERIMENT = { budget_accelerator_hours: 10, duration_hours: 48, researchers: 2 }

// Numbers are held as typed, for the reason StageDraft gives, and parsed on submit.
interface FormState {
  name: string
  description: string
  budget_accelerator_hours: string
  // The researchers the platform runs, sent as max_agents: nobody else joins a hosted run.
  max_agents: string
  // Empty is the platform's default (quota.default_max_concurrent_accelerators).
  max_concurrent_accelerators: string
  // Empty is no start time: the platform starts it once its baseline is declared.
  starts_at: string
  duration_hours: string
  metrics: MetricDefinition[]
  // Empty is the platform's default.
  report_interval_seconds: string
  // null is the platform's default ladder.
  stages: StageDraft[] | null
  repository: string
  agent_image: string
}

// StageDraft holds what's typed, not what's parsed. A number-typed field cannot represent a
// half-edited value: clearing it yields "", Number("") is 0, and the input snaps back to 0 before
// the next digit is typed. Strings let a field be empty while editing; the ladder is parsed once,
// on submit.
type StageDraft = { length_pct: string; evict_pct: string; max_job_hours: string }

const ONE_STAGE: StageDraft[] = [{ length_pct: '100', evict_pct: '0', max_job_hours: '' }]

function toDrafts(stages: Stage[]): StageDraft[] {
  return stages.map(s => ({
    length_pct: String(s.length_pct),
    evict_pct: String(s.evict_pct),
    max_job_hours: s.max_job_hours ? String(s.max_job_hours) : '',
  }))
}

// An empty max_job_hours means "no limit" and parses to 0; an empty length/evict is a genuine
// gap that stagesError reports.
function toStages(drafts: StageDraft[]): Stage[] {
  return drafts.map(s => ({
    length_pct: Number(s.length_pct),
    evict_pct: Number(s.evict_pct),
    max_job_hours: s.max_job_hours.trim() === '' ? 0 : Number(s.max_job_hours),
  }))
}

// stagesError mirrors domain.ValidateStages so the form rejects a bad ladder before the API does.
function stagesError(stages: StageDraft[]): string | null {
  if (stages.length === 0) return 'Add at least one stage.'
  if (stages.length > 8) return 'At most 8 stages.'
  if (stages.some(s => s.length_pct.trim() === '' || s.evict_pct.trim() === '')) return 'Every stage needs a length % and an evict %.'
  if (stages.some(s => !Number.isFinite(Number(s.length_pct)) || !Number.isFinite(Number(s.evict_pct)))) return 'Stage percentages must be numbers.'
  const total = stages.reduce((s, x) => s + Number(x.length_pct), 0)
  if (stages.some(s => Number(s.length_pct) <= 0)) return 'Every stage must be longer than 0%.'
  if (stages.some(s => Number(s.evict_pct) < 0 || Number(s.evict_pct) >= 100)) return 'Evict % must be between 0 and 99.'
  if (stages.some(s => s.max_job_hours.trim() !== '' && (!Number.isFinite(Number(s.max_job_hours)) || Number(s.max_job_hours) < 0))) return 'Max job hours must be a positive number, or empty for no limit.'
  if (total < 99.999 || total > 100.001) return `Stage lengths must add up to 100% (now ${round2(total)}%).`
  if (Number(stages[stages.length - 1].evict_pct) !== 0) return 'The final stage evicts 0%: nothing follows it.'
  return null
}

// Length totals are summed from free-typed decimals; 33.33+33.33+33.34 must read as 100, not
// 99.99999999999999.
function round2(n: number): number {
  return Math.round(n * 100) / 100
}

/** The detail behind a one-line hint, on hover and to a screen reader. */
function More({ children }: { children: string }) {
  return (
    <span role="img" aria-label={children} title={children} className="ml-1 inline-flex cursor-help align-[-2px] text-subtle">
      <Info size={13} aria-hidden="true" />
    </span>
  )
}

function StagesEditor({ stages, onChange }: { stages: StageDraft[]; onChange: (s: StageDraft[]) => void }) {
  const err = stagesError(stages)
  const total = round2(stages.reduce((s, x) => s + Number(x.length_pct || 0), 0))

  function setStage(i: number, patch: Partial<StageDraft>) {
    onChange(stages.map((s, idx) => idx === i ? { ...s, ...patch } : s))
  }

  return (
    <div className="grid gap-2">
      <div className="grid grid-cols-[1.25rem_1fr_1fr_1fr_1.25rem] items-center gap-2">
        <span />
        <Eyebrow>Length %</Eyebrow>
        <Eyebrow>Evict %</Eyebrow>
        <Eyebrow>Max job hours</Eyebrow>
        <span />
        {stages.map((s, i) => {
          const last = i === stages.length - 1
          return (
            <React.Fragment key={i}>
              <span className="text-xs tabular-nums text-subtle">{i + 1}</span>
              <Input
                className="w-full min-w-0" type="number" min={0.1} max={100} step="any" value={s.length_pct}
                aria-label={`Stage ${i + 1} length %`}
                onChange={e => setStage(i, { length_pct: e.target.value })}
              />
              {last ? (
                <span className="px-3 text-sm text-muted" title="Nothing follows the final stage">0</span>
              ) : (
                <Input
                  className="w-full min-w-0" type="number" min={0} max={99} step={5} value={s.evict_pct}
                  aria-label={`Stage ${i + 1} evict %`}
                  onChange={e => setStage(i, { evict_pct: e.target.value })}
                />
              )}
              <Input
                className="w-full min-w-0" type="number" min={0} step={0.5} value={s.max_job_hours}
                aria-label={`Stage ${i + 1} max job hours`}
                placeholder="no limit"
                onChange={e => setStage(i, { max_job_hours: e.target.value })}
              />
              <Button
                type="button"
                variant="ghost"
                size="icon"
                onClick={() => onChange(stages.filter((_, idx) => idx !== i).map((st, idx, arr) => idx === arr.length - 1 ? { ...st, evict_pct: '0' } : st))}
                disabled={stages.length <= 1}
                title="Remove this stage"
              >×</Button>
            </React.Fragment>
          )
        })}
      </div>

      <div className="flex items-center gap-3">
        <Button
          type="button" size="sm"
          disabled={stages.length >= 8}
          onClick={() => onChange([
            ...stages.slice(0, -1),
            { ...stages[stages.length - 1], evict_pct: '25' },
            { length_pct: '', evict_pct: '0', max_job_hours: '' },
          ])}
        >+ Add stage</Button>
        <span className={clsx('text-xs tabular-nums', Math.abs(total - 100) < 0.001 ? 'text-success' : 'text-warning')}>
          Total {total}%
        </span>
      </div>

      {err && <p className="text-xs text-warning">{err}</p>}
    </div>
  )
}

const arrow = (direction: MetricDefinition['direction']) => direction === 'maximize' ? '↑ max' : '↓ min'

/** One control to add a metric: pick a common one, or type a key of your own, which maximizes until flipped. */
function MetricPicker({ taken, onAdd }: { taken: string[]; onAdd: (m: MetricDefinition) => void }) {
  const [query, setQuery] = useState('')
  const typed = query.trim()
  const common = COMMON_ML_METRICS.filter(m => m.key !== 'custom' && !taken.includes(m.key)
    && (typed === '' || `${m.key} ${m.label}`.toLowerCase().includes(typed.toLowerCase())))
  const custom = typed !== '' && !taken.includes(typed) && !COMMON_ML_METRICS.some(m => m.key === typed)

  return (
    <Combobox<MetricDefinition | null>
      immediate
      value={null}
      onChange={m => { if (m) { onAdd(m); setQuery('') } }}
      onClose={() => setQuery('')}
    >
      <ComboboxInput
        aria-label="Add a metric"
        className={clsx(fieldClass(), 'w-full')}
        placeholder="Add a metric: pick one or type its key"
        displayValue={() => query}
        onChange={e => setQuery(e.target.value)}
      />
      <ComboboxOptions anchor={{ to: 'bottom start', gap: 4, padding: 8 }} className={panelClass('input')}>
        {custom && (
          <ComboboxOption value={{ key: typed, direction: 'maximize' }} className={optionClass()}>
            <OptionLabel badge={arrow('maximize')}>Add {typed}</OptionLabel>
          </ComboboxOption>
        )}
        {common.map(m => (
          <ComboboxOption key={m.key} value={{ key: m.key, direction: m.direction }} className={optionClass()}>
            <OptionLabel badge={arrow(m.direction)}>{m.label}</OptionLabel>
          </ComboboxOption>
        ))}
      </ComboboxOptions>
    </Combobox>
  )
}

function MetricsEditor({ metrics, onChange }: { metrics: MetricDefinition[]; onChange: (m: MetricDefinition[]) => void }) {
  function toggleDir(key: string) {
    onChange(metrics.map(m => m.key === key ? { ...m, direction: m.direction === 'maximize' ? 'minimize' : 'maximize' } : m))
  }

  function makePrimary(key: string) {
    const picked = metrics.find(m => m.key === key)
    if (picked) onChange([picked, ...metrics.filter(m => m.key !== key)])
  }

  return (
    <div className="grid gap-2.5">
      <MetricPicker taken={metrics.map(m => m.key)} onAdd={m => onChange([...metrics, m])} />
      {metrics.length > 0 && (
        <div className="flex flex-wrap gap-1.5">
          {metrics.map((m, i) => (
            <Pill key={m.key} tone={m.direction === 'maximize' ? 'success' : 'warning'} active={i === 0}>
              {i === 0
                ? <span title="Primary: ranks the leaderboard">★</span>
                : <Button type="button" variant="ghost" size="icon" onClick={() => makePrimary(m.key)} title="Make primary">☆</Button>}
              <Button type="button" variant="ghost" size="icon" onClick={() => toggleDir(m.key)} title="Flip between maximize and minimize">
                {m.direction === 'maximize' ? '↑' : '↓'}
              </Button>
              {m.key}
              <Button type="button" variant="ghost" size="icon" onClick={() => onChange(metrics.filter(x => x.key !== m.key))} title="Remove this metric">×</Button>
            </Pill>
          ))}
        </div>
      )}
    </div>
  )
}

const STEPS = [
  { id: '1', label: '1. What' },
  { id: '2', label: '2. Measure' },
  { id: '3', label: '3. Budget and rules' },
] as const
type StepID = typeof STEPS[number]['id']

const LOCKED = 'Locked once running.'

interface FormProps {
  /** The experiment to edit. Without one the form creates one in the selected project. */
  initial?: PlatformExperiment | null
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
export function PlatformExperimentForm({ initial, budgetKind, onSaved, onCancel }: FormProps) {
  const { project } = useTenant()
  const needsGitHub = useNeedsGitHub()
  const isEdit = !!initial
  // Once running, admission and the stage ladder have acted on everything but the name and the
  // description, and the API takes no other field, so the rest is locked here.
  const lockedWhileRunning = isEdit && initial?.status === PlatformExperimentStatus.RUNNING
  const [form, setForm] = useState<FormState>({
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
  })
  // The form as it opened. An edit sends the fields that differ from it.
  const [opened] = useState(form)
  const [step, setStep] = useState<StepID>('1')
  // What the chosen repository's experiment file did to the form: filled it, is not there, or
  // could not be read.
  const [fromFile, setFromFile] = useState<{ repository: string; state: 'reading' | 'filled' | 'none' | 'failed'; message?: string } | null>(null)
  const [submitting, setSubmitting] = useState(false)
  const [error, setError] = useState<string | null>(null)
  // What the API said of a new repository GitHub would not give it, shown where one is chosen.
  const [notCreated, setNotCreated] = useState<string | null>(null)

  function set<K extends keyof FormState>(field: K, value: FormState[K]) {
    setForm(f => ({ ...f, [field]: value }))
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
    setNotCreated(null)
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
    if (!isEdit && !form.description.trim()) return 'Description is required: it is the brief every agent works from.'
    if (Number(form.duration_hours) <= 0) return 'Duration must be greater than 0.'
    if (Number(form.budget_accelerator_hours) <= 0) return 'Budget must be greater than 0.'
    if (!isEdit && Number(form.max_agents) < 1) return 'At least one researcher.'
    return null
  }

  // What an edit changed, as the fields of the update. A date is compared as the form holds it,
  // which is to the minute, so one left alone is never sent back rounded.
  function changes(): PlatformExperimentPatch {
    const patch: PlatformExperimentPatch = {}
    if (form.name.trim() !== opened.name) patch.name = form.name.trim()
    if (form.description.trim() !== opened.description) patch.description = form.description.trim()
    if (lockedWhileRunning) return patch
    if (Number(form.budget_accelerator_hours) !== Number(opened.budget_accelerator_hours)) patch.budget_accelerator_hours = Number(form.budget_accelerator_hours)
    if (Number(form.max_agents) !== Number(opened.max_agents)) patch.max_agents = Number(form.max_agents)
    if (form.max_concurrent_accelerators !== opened.max_concurrent_accelerators) patch.max_concurrent_accelerators = Number(form.max_concurrent_accelerators)
    if (JSON.stringify(form.metrics) !== JSON.stringify(opened.metrics)) patch.metrics = form.metrics
    if (form.report_interval_seconds !== opened.report_interval_seconds) patch.report_interval_seconds = Number(form.report_interval_seconds)
    if (form.starts_at !== opened.starts_at) patch.starts_at = new Date(form.starts_at).toISOString()
    if (Number(form.duration_hours) !== Number(opened.duration_hours)) patch.duration_hours = Number(form.duration_hours)
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
    setNotCreated(null)
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
      const { experiment, failure } = await createAndRun(project.id, payload, Number(form.max_agents))
      onSaved(experiment, { failure })
    } catch (err: unknown) {
      if (repositoryNotProvided(err)) {
        setNotCreated(err.message)
        setStep('1')
      } else {
        setError(err instanceof Error ? err.message : String(err))
      }
    } finally {
      setSubmitting(false)
    }
  }

  const at = STEPS.findIndex(s => s.id === step)
  const next = STEPS[at + 1]
  const back = STEPS[at - 1]
  const fileNote = fromFile?.repository === form.repository ? fromFile : null

  return (
    <>
      <div className="mb-5">
        <Tabs label="Steps" tabs={[...STEPS]} value={step} onChange={go} />
      </div>

      <form onSubmit={handleSubmit}>
        <div className="flex flex-col gap-5">
          {step === '1' && (
            <>
              {isEdit ? (
                <FieldGroup label="Repository and agent image" hint="Fixed when the experiment was created.">
                  <p className="text-sm">{form.repository || 'No repository'} · {form.agent_image || 'the platform\'s agent image'}</p>
                </FieldGroup>
              ) : (
                <FormField
                  label="Repository"
                  hint={<>The code agents work on. Fixed once created.<More>A repository with .hypothesisloop/experiment.yaml fills in the rest of this form.</More></>}
                >
                  <RepositorySelect value={form.repository} onChange={r => void chooseRepository(r)} notCreated={notCreated ?? undefined} />
                  {fileNote?.state === 'reading' && <p className="text-xs text-subtle">Reading .hypothesisloop/experiment.yaml of {fileNote.repository}…</p>}
                  {fileNote?.state === 'filled' && <p className="text-xs text-muted">Filled in from .hypothesisloop/experiment.yaml of {fileNote.repository}.</p>}
                  {fileNote?.state === 'none' && <p className="text-xs text-muted">{fileNote.repository} has no .hypothesisloop/experiment.yaml.</p>}
                  {fileNote?.state === 'failed' && <InlineError>Could not read .hypothesisloop/experiment.yaml of {fileNote.repository}: {fileNote.message}</InlineError>}
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
          )}

          {step === '2' && (
            <>
              <FieldGroup
                label="Metrics"
                hint={lockedWhileRunning ? LOCKED : <>★ ranks the leaderboard; ↑↓ flips a direction.<More>At a stage cut an agent survives if it survives on at least one metric, so a specialist is not cut for a metric it never targeted.</More></>}
              >
                {lockedWhileRunning ? (
                  <div className="flex flex-wrap gap-1.5">
                    {form.metrics.map(m => <Pill key={m.key}>{m.key} {m.direction === 'maximize' ? '↑' : '↓'}</Pill>)}
                  </div>
                ) : (
                  <MetricsEditor metrics={form.metrics} onChange={m => set('metrics', m)} />
                )}
              </FieldGroup>
              <FormField label="Duration (hours)" hint={lockedWhileRunning ? LOCKED : 'Counted from the start.'}>
                <Input type="number" className="w-40" min={0} step="any" value={form.duration_hours} disabled={lockedWhileRunning} onChange={e => set('duration_hours', e.target.value)} />
              </FormField>
            </>
          )}

          {step === '3' && (
            <>
              <div className="grid grid-cols-2 gap-4">
                <FormField label="Researchers" hint={lockedWhileRunning ? LOCKED : isEdit ? 'Nobody joins once it has started.' : 'Run by the platform, with a coordinator.'}>
                  <Input type="number" min={1} max={500} step={1} value={form.max_agents} disabled={lockedWhileRunning} onChange={e => set('max_agents', e.target.value)} />
                </FormField>
                <FormField
                  label="Budget (accelerator hours)"
                  hint={lockedWhileRunning ? LOCKED : <>Caps everything it spends.<More>{`Counted in ${budgetKind ? budgetUnit(budgetKind, 'long') : `${budgetUnit('accelerator', 'long')}, or ${budgetUnit('cpu', 'long')} for jobs on CPU`}. An hour on an accelerator spends its type's price; memory and storage spend nothing.`}</More></>}
                >
                  <Input type="number" min={1} step={0.5} value={form.budget_accelerator_hours} disabled={lockedWhileRunning} onChange={e => set('budget_accelerator_hours', e.target.value)} />
                </FormField>
              </div>

              <FieldGroup
                label="Stage ladder"
                hint={isEdit ? 'Fixed at creation.' : <>Each stage ends by cutting the weakest share of agents.<More>Lengths add up to 100% and the final stage cuts nobody. Cuts are skipped at 4 agents or fewer. Max job hours caps how long one job runs in a stage.</More></>}
              >
                {isEdit ? (
                  <div className="flex flex-wrap gap-1.5">
                    {form.stages
                      ? form.stages.map((s, i) => (
                        <Pill key={i}>
                          {i + 1}: {s.length_pct}% {Number(s.evict_pct) > 0 ? `→ cut ${s.evict_pct}%` : '→ no cut'}{s.max_job_hours ? ` · ≤${s.max_job_hours}h/job` : ''}
                        </Pill>
                      ))
                      : <Pill>Platform default</Pill>}
                  </div>
                ) : (
                  <div className="grid gap-3">
                    <Segmented
                      label="Stage ladder"
                      size="sm"
                      options={[{ value: 'platform', label: 'Platform default' }, { value: 'custom', label: 'Custom' }]}
                      value={form.stages ? 'custom' : 'platform'}
                      onChange={v => set('stages', v === 'custom' ? ONE_STAGE : null)}
                    />
                    {form.stages && <StagesEditor stages={form.stages} onChange={s => set('stages', s)} />}
                  </div>
                )}
              </FieldGroup>

              <Disclosure label="More settings">
                <div className="flex flex-col gap-4">
                  {!isEdit && (
                    <FormField label="Agent image" hint="Empty is the platform's image; a custom one is built FROM it.">
                      <Input value={form.agent_image} onChange={e => set('agent_image', e.target.value)} placeholder="registry.example.com/team/agent:tag" />
                    </FormField>
                  )}
                  {isEdit && (
                    <FormField label="Starts at" hint={lockedWhileRunning ? LOCKED : 'Empty starts it once its baseline is declared.'}>
                      <Input type="datetime-local" value={form.starts_at} disabled={lockedWhileRunning} onChange={e => set('starts_at', e.target.value)} />
                    </FormField>
                  )}
                  <FormField label="Max concurrent accelerators" hint={lockedWhileRunning ? LOCKED : "Held at once across its jobs. Empty is the platform's default."}>
                    <Input type="number" min={1} step={1} placeholder="platform default" value={form.max_concurrent_accelerators} disabled={lockedWhileRunning} onChange={e => set('max_concurrent_accelerators', e.target.value)} />
                  </FormField>
                  <FormField label="Report interval (seconds)" hint={lockedWhileRunning ? LOCKED : "How often jobs report metrics. Empty is the platform's default."}>
                    <Input type="number" min={1} step={1} placeholder="platform default" value={form.report_interval_seconds} disabled={lockedWhileRunning} onChange={e => set('report_interval_seconds', e.target.value)} />
                  </FormField>
                </div>
              </Disclosure>
            </>
          )}

          {error && <ErrorMessage>{error}</ErrorMessage>}

          <div className="mt-1 flex items-center gap-2">
            {onCancel && <Button type="button" onClick={onCancel}>Cancel</Button>}
            <span className="flex-1" />
            {back && <Button type="button" onClick={() => go(back.id)}>Back</Button>}
            {next && <Button type="button" variant={isEdit ? 'default' : 'primary'} onClick={() => go(next.id)}>Next</Button>}
            {(isEdit || !next) && (!isEdit && needsGitHub ? <ConnectGitHubStep /> : (
              <Button type="submit" variant="primary" loading={submitting} disabled={!!ladderError}>
                {isEdit ? 'Save changes' : 'Create experiment'}
              </Button>
            ))}
          </div>
        </div>
      </form>
    </>
  )
}

/** The form in a dialog, which closes once the experiment is saved. */
export function PlatformExperimentDialog({ initial, onSaved, onClose }: {
  initial?: PlatformExperiment | null
  onSaved: FormProps['onSaved']
  onClose: () => void
}) {
  return (
    <Dialog open onClose={onClose} title={initial ? 'Edit experiment' : 'New experiment'}>
      <PlatformExperimentForm initial={initial} onCancel={onClose} onSaved={(saved, run) => { onSaved(saved, run); onClose() }} />
    </Dialog>
  )
}

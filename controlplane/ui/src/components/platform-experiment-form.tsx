'use client'

import React, { useState } from 'react'
import { clsx } from 'clsx'
import { ApiError, NO_EXPERIMENT_FILE, createPlatformExperiment, fetchExperimentFile, repositoryNotProvided, updatePlatformExperiment } from '@/lib/api'
import type { PlatformExperimentPatch } from '@/lib/api'
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
import { FieldGroup, FormField } from '@/components/ui/form-field'
import { Input, Textarea } from '@/components/ui/input'
import { Select } from '@/components/ui/select'
import { Segmented, Tabs } from '@/components/ui/tabs'
import { Eyebrow, Pill } from '@/components/ui/text'
import { ErrorMessage, InlineError } from '@/components/ui/status-message'

interface FormState {
  name: string
  description: string
  budget_accelerator_hours: number
  // AccH is the only budgeted dimension. CPU/RAM/storage are submitted as 0 (untracked) and are
  // bounded per job by the platform's max_*_per_job admission caps instead — a second budget to
  // size and split buys nothing the caps don't already give.
  max_agents: number
  // Accelerators-in-flight (SUBMITTED+RUNNING) this experiment may hold at once. Empty string
  // means "use the platform default" — see quota.default_max_concurrent_accelerators.
  max_concurrent_accelerators: string
  // Empty is no start time: the platform starts it once its baseline is declared.
  starts_at: string
  duration_hours: number
  metrics: MetricDefinition[]
  // Empty is the platform's default.
  report_interval_seconds: string
  stages: StageDraft[]
  repository: string
  agent_image: string
  run_by: RunBy
  researchers: number
}

// Who runs the agents of a new experiment. 'platform' creates it with a hosted coordinator, adds
// hosted agents; 'operator' creates it and leaves the rest to its page.
type RunBy = 'platform' | 'operator'

const RUN_BY_OPTIONS: Array<{ value: RunBy; label: string }> = [
  { value: 'operator', label: 'I run my own agents' },
  { value: 'platform', label: 'Hosted agents' },
]

const RUN_BY_HINTS: Record<RunBy, string> = {
  operator: 'The experiment is created and nothing else. You add agents on its page, run by you or by the platform, and the platform starts it once its baseline is declared.',
  platform: 'The platform runs a coordinator and this many researcher agents on machines of its own, and the experiment starts as soon as they have joined. How many it runs for one experiment is the platform\'s limit, and it says so when it is passed.',
}

// StageDraft holds what's typed, not what's parsed. A number-typed field cannot represent a
// half-edited value: clearing it yields "", Number("") is 0, and the input snaps back to 0 before
// the next digit is typed. Strings let a field be empty while editing; the ladder is parsed once,
// on submit.
type StageDraft = { length_pct: string; evict_pct: string; max_job_hours: string }

const DEFAULT_STAGES: StageDraft[] = [
  { length_pct: '40', evict_pct: '75', max_job_hours: '' },
  { length_pct: '60', evict_pct: '0', max_job_hours: '' },
]

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
  if (total < 99.999 || total > 100.001) return `Stage lengths must add up to 100% (currently ${round2(total)}%).`
  if (Number(stages[stages.length - 1].evict_pct) !== 0) return 'The final stage must evict 0% — nothing follows it.'
  return null
}

// Length totals are summed from free-typed decimals; 33.33+33.33+33.34 must read as 100, not
// 99.99999999999999.
function round2(n: number): number {
  return Math.round(n * 100) / 100
}

function StagesEditor({ stages, onChange }: { stages: StageDraft[]; onChange: (s: StageDraft[]) => void }) {
  const err = stagesError(stages)
  const total = round2(stages.reduce((s, x) => s + Number(x.length_pct || 0), 0))

  function setStage(i: number, patch: Partial<StageDraft>) {
    onChange(stages.map((s, idx) => idx === i ? { ...s, ...patch } : s))
  }

  return (
    <div>
      <div className="mb-2 flex flex-wrap gap-1.5">
        <Button type="button" size="sm" onClick={() => onChange(DEFAULT_STAGES)}>Platform default (40/75, 60/0)</Button>
        <Button type="button" size="sm" onClick={() => onChange([{ length_pct: '100', evict_pct: '0', max_job_hours: '' }])}>Single stage, no cuts</Button>
        <Button type="button" size="sm" onClick={() => onChange([{ length_pct: '20', evict_pct: '50', max_job_hours: '' }, { length_pct: '30', evict_pct: '25', max_job_hours: '' }, { length_pct: '50', evict_pct: '0', max_job_hours: '' }])}>Three stages</Button>
        <Button type="button" size="sm" onClick={() => onChange([{ length_pct: '30', evict_pct: '25', max_job_hours: '1' }, { length_pct: '70', evict_pct: '0', max_job_hours: '' }])}>Explore then commit (1h cap, then none)</Button>
      </div>

      <div className="grid grid-cols-[auto_1fr_1fr_1fr_auto] items-center gap-2">
        <Eyebrow>#</Eyebrow>
        <Eyebrow>Length %</Eyebrow>
        <Eyebrow>Evict %</Eyebrow>
        <Eyebrow>Max job h</Eyebrow>
        <span />
        {stages.map((s, i) => (
          <React.Fragment key={i}>
            <span className="font-mono text-xs text-subtle">{i + 1}</span>
            <Input
              className="w-full min-w-0" type="number" min={0.1} max={100} step="any" value={s.length_pct}
              onChange={e => setStage(i, { length_pct: e.target.value })}
            />
            <Input
              className="w-full min-w-0"
              type="number" min={0} max={99} step={5}
              value={s.evict_pct}
              disabled={i === stages.length - 1}
              title={i === stages.length - 1 ? 'The final stage cannot evict — nothing follows it' : undefined}
              onChange={e => setStage(i, { evict_pct: e.target.value })}
            />
            <Input
              className="w-full min-w-0" type="number" min={0} step={0.5} value={s.max_job_hours}
              placeholder="no limit"
              title="Longest a single job may run while this stage is current. Leave empty for no limit."
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
        ))}
      </div>

      <div className="mt-2 flex items-center gap-2">
        <Button
          type="button" size="sm"
          disabled={stages.length >= 8}
          onClick={() => onChange([
            ...stages.slice(0, -1),
            { ...stages[stages.length - 1], evict_pct: '25' },
            { length_pct: '', evict_pct: '0', max_job_hours: '' },
          ])}
        >+ Add stage</Button>
        <span className={clsx('font-mono text-2xs', Math.abs(total - 100) < 0.001 ? 'text-success' : 'text-warning')}>
          total {total}%
        </span>
      </div>

      {err && <p className="mt-1.5 text-2xs text-warning">{err}</p>}
    </div>
  )
}

const DIRECTION_OPTIONS: { value: 'maximize' | 'minimize'; label: string }[] = [
  { value: 'maximize', label: '↑ max' },
  { value: 'minimize', label: '↓ min' },
]

function MetricsEditor({ metrics, onChange }: { metrics: MetricDefinition[]; onChange: (m: MetricDefinition[]) => void }) {
  const [customKey, setCustomKey] = useState('')
  const [customDir, setCustomDir] = useState<'maximize' | 'minimize'>('maximize')
  const [selectedPreset, setSelectedPreset] = useState('')

  function addPreset() {
    if (!selectedPreset) return
    if (selectedPreset === 'custom') return // handled separately
    const preset = COMMON_ML_METRICS.find(m => m.key === selectedPreset)
    if (!preset || metrics.some(m => m.key === preset.key)) return
    onChange([...metrics, { key: preset.key, direction: preset.direction }])
    setSelectedPreset('')
  }

  function addCustom() {
    const key = customKey.trim()
    if (!key || metrics.some(m => m.key === key)) return
    onChange([...metrics, { key, direction: customDir }])
    setCustomKey('')
  }

  function remove(key: string) {
    onChange(metrics.filter(m => m.key !== key))
  }

  function toggleDir(key: string) {
    onChange(metrics.map(m => m.key === key ? { ...m, direction: m.direction === 'maximize' ? 'minimize' : 'maximize' } : m))
  }

  function makePrimary(key: string) {
    const idx = metrics.findIndex(m => m.key === key)
    if (idx <= 0) return
    const reordered = [...metrics]
    const [picked] = reordered.splice(idx, 1)
    reordered.unshift(picked)
    onChange(reordered)
  }

  return (
    <div>
      <div className="mb-2 flex gap-1.5">
        <Select
          aria-label="Common metric"
          className="min-w-0 flex-1"
          placeholder="Pick a common metric"
          value={selectedPreset}
          onChange={setSelectedPreset}
          options={COMMON_ML_METRICS.filter(m => m.key !== 'custom').map(m => ({
            value: m.key, label: `${m.label} (${m.direction === 'maximize' ? '↑ max' : '↓ min'})`,
          }))}
        />
        <Button type="button" variant="primary" size="sm" onClick={addPreset}>Add</Button>
      </div>

      <div className="mb-1.5 flex gap-1.5">
        <Input
          className="min-w-0 flex-1"
          placeholder="custom_metric_key"
          value={customKey}
          onChange={e => setCustomKey(e.target.value)}
        />
        <Select aria-label="Direction of the custom metric" value={customDir} onChange={setCustomDir} options={DIRECTION_OPTIONS} />
        <Button type="button" size="sm" onClick={addCustom}>Add Custom</Button>
      </div>

      {metrics.length > 0 && (
        <div className="mt-2 flex flex-wrap gap-1.5">
          {metrics.map((m, i) => {
            const primary = i === 0
            return (
              <Pill key={m.key} tone={m.direction === 'maximize' ? 'success' : 'warning'} active={primary}>
                {primary ? (
                  <span title="Primary metric — used for ranking" className="opacity-80">★</span>
                ) : (
                  <Button type="button" variant="ghost" size="icon" onClick={() => makePrimary(m.key)} title="Make primary">☆</Button>
                )}
                <Button type="button" variant="ghost" size="icon" onClick={() => toggleDir(m.key)} title="Flip between maximize and minimize">
                  {m.direction === 'maximize' ? '↑' : '↓'}
                </Button>
                {m.key}
                <Button type="button" variant="ghost" size="icon" onClick={() => remove(m.key)} title="Remove this metric">×</Button>
              </Pill>
            )
          })}
        </div>
      )}
    </div>
  )
}

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
 * The one form a platform experiment is created and edited with, in two steps. It checks only
 * what it needs to build the request; every other rule is the API's, whose refusal it shows.
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
    budget_accelerator_hours: initial?.budget_accelerator_hours ?? 10,
    max_agents: initial?.max_agents ?? 20,
    max_concurrent_accelerators: initial?.max_concurrent_accelerators != null ? String(initial.max_concurrent_accelerators) : '',
    starts_at: !isZeroDate(initial?.starts_at) ? new Date(initial!.starts_at!).toISOString().slice(0, 16) : '',
    duration_hours: initial?.duration_hours ?? 48,
    metrics: initial?.metrics ?? [],
    report_interval_seconds: initial?.report_interval_seconds ? String(initial.report_interval_seconds) : '',
    stages: initial?.stages?.length ? toDrafts(initial.stages) : DEFAULT_STAGES,
    repository: initial?.repository ?? '',
    agent_image: initial?.agent_image ?? '',
    run_by: 'operator',
    researchers: 2,
  })
  // The form as it opened. An edit sends the fields that differ from it.
  const [opened] = useState(form)
  const [step, setStep] = useState<1 | 2>(1)
  // What the chosen repository's experiment file did to the form: filled it, is not there, or
  // could not be read.
  const [fromFile, setFromFile] = useState<{ repository: string; state: 'reading' | 'filled' | 'none' | 'failed'; message?: string } | null>(null)
  const [submitting, setSubmitting] = useState(false)
  const [error, setError] = useState<string | null>(null)
  // What the API said of a new repository GitHub would not give it, shown where one is chosen.
  const [notCreated, setNotCreated] = useState<string | null>(null)

  // With hosted agents the experiment has the researchers asked for and nobody else joins, so
  // the cap follows their number until the person states one.
  const [maxAgentsStated, setMaxAgentsStated] = useState(false)

  function set(field: keyof FormState, value: string | number | MetricDefinition[] | StageDraft[] | RunBy) {
    setForm(f => ({ ...f, [field]: value }))
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

  const ladderError = stagesError(form.stages)
  const hosted = !isEdit && form.run_by === 'platform'
  const maxAgents = hosted && !maxAgentsStated ? form.researchers : form.max_agents

  function step1Error(): string | null {
    if (!form.name.trim()) return 'Name is required.'
    if (!isEdit && !form.description.trim()) return 'Description is required: it is the brief every agent works from.'
    if (Number(form.duration_hours) <= 0) return 'Duration must be greater than 0.'
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
    if (form.max_concurrent_accelerators.trim() !== '' && form.max_concurrent_accelerators !== opened.max_concurrent_accelerators) patch.max_concurrent_accelerators = Number(form.max_concurrent_accelerators)
    if (JSON.stringify(form.metrics) !== JSON.stringify(opened.metrics)) patch.metrics = form.metrics
    if (form.report_interval_seconds.trim() !== '' && form.report_interval_seconds !== opened.report_interval_seconds) patch.report_interval_seconds = Number(form.report_interval_seconds)
    if (form.starts_at && form.starts_at !== opened.starts_at) patch.starts_at = new Date(form.starts_at).toISOString()
    if (Number(form.duration_hours) !== Number(opened.duration_hours)) patch.duration_hours = Number(form.duration_hours)
    return patch
  }

  // Lives on step 2 now (moved alongside Max researchers), so it no longer gates the step 1→2
  // transition — only the final submit, same as the ladder.
  function step2BudgetError(): string | null {
    return Number(form.budget_accelerator_hours) <= 0 ? 'Total compute budget must be greater than 0.' : null
  }

  async function handleSubmit(e: React.FormEvent) {
    e.preventDefault()
    // Both steps are validated here, not just the visible one — a bad ladder on step 2 must not
    // be submittable just because step 1 passed. Tab navigation itself is never gated — only
    // submit is, so the user is free to jump between tabs while filling the form out of order.
    const err = step1Error() ?? step2BudgetError() ?? (isEdit ? null : ladderError)
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
      const payload = {
        name: form.name.trim(),
        description: form.description.trim(),
        budget_accelerator_hours: Number(form.budget_accelerator_hours),
        max_agents: Number(maxAgents),
        ...(form.max_concurrent_accelerators.trim() !== '' ? { max_concurrent_accelerators: Number(form.max_concurrent_accelerators) } : {}),
        metrics: form.metrics,
        ...(form.report_interval_seconds.trim() !== '' ? { report_interval_seconds: Number(form.report_interval_seconds) } : {}),
        stages: toStages(form.stages),
        ...repositoryFields(form.repository),
        ...(form.agent_image.trim() ? { agent_image: form.agent_image.trim() } : {}),
        ...(!hosted && form.starts_at ? { starts_at: new Date(form.starts_at).toISOString() } : {}),
        duration_hours: Number(form.duration_hours),
      }
      if (hosted) {
        const { experiment, failure } = await createAndRun(project.id, payload, Number(form.researchers))
        onSaved(experiment, { failure })
      } else {
        onSaved(await createPlatformExperiment(project.id, payload))
      }
    } catch (err: unknown) {
      if (repositoryNotProvided(err)) {
        setNotCreated(err.message)
        setStep(1)
      } else {
        setError(err instanceof Error ? err.message : String(err))
      }
    } finally {
      setSubmitting(false)
    }
  }

  return (
    <>
        <div className="mb-4">
          <Tabs
            label="Steps"
            tabs={[{ id: '1', label: '1. Essentials' }, { id: '2', label: '2. Budget and rules' }]}
            value={String(step) as '1' | '2'}
            onChange={id => { setError(null); setStep(id === '1' ? 1 : 2) }}
          />
        </div>

        <form onSubmit={handleSubmit}>
          <div className="flex flex-col gap-4">
            {step === 1 ? (
              <>
                {isEdit ? (
                  <FieldGroup label="Repository and agent image" hint="Fixed when the experiment was created.">
                    <p className="font-mono text-xs">
                      {form.repository || 'no repository'} · {form.agent_image || 'the platform\'s agent image'}
                    </p>
                  </FieldGroup>
                ) : (
                  <>
                    <FormField
                      label="Repository"
                      hint="Optional. The code this experiment works on: agents clone it and push their branches to it, and jobs check it out. A repository with a .hypothesisloop/experiment.yaml fills in the rest of this form. Left empty, the platform creates a private repository for it on your connected GitHub account. Fixed once created."
                    >
                      <RepositorySelect value={form.repository} onChange={r => void chooseRepository(r)} notCreated={notCreated ?? undefined} />
                    </FormField>
                    {fromFile?.repository === form.repository && fromFile.state === 'reading' && (
                      <p className="text-xs text-subtle">Reading .hypothesisloop/experiment.yaml of {fromFile.repository}…</p>
                    )}
                    {fromFile?.repository === form.repository && fromFile.state === 'filled' && (
                      <p className="text-xs text-muted">Filled in from .hypothesisloop/experiment.yaml of {fromFile.repository}: the name, the description, the metrics and the settings it states. Change anything before you create it.</p>
                    )}
                    {fromFile?.repository === form.repository && fromFile.state === 'none' && (
                      <p className="text-xs text-muted">{fromFile.repository} has no .hypothesisloop/experiment.yaml, so nothing is filled in: describe the task and name its metrics below.</p>
                    )}
                    {fromFile?.repository === form.repository && fromFile.state === 'failed' && (
                      <InlineError>Could not read .hypothesisloop/experiment.yaml of {fromFile.repository}: {fromFile.message}</InlineError>
                    )}
                  </>
                )}
                <FormField label="Name *" hint="Shown to every agent in the listing. Make it specific enough to tell two runs apart.">
                  <Input value={form.name} onChange={e => set('name', e.target.value)} placeholder="e.g. Q3 Vision Benchmark" />
                </FormField>

                <FormField
                  label="Description"
                  hint="The brief agents actually read before choosing an approach: the goal, the dataset, the evaluation protocol, and any constraint on how they may reach it."
                >
                  <Textarea
                    className="min-h-20"
                    value={form.description}
                    onChange={e => set('description', e.target.value)}
                    placeholder="Goals, evaluation protocol, dataset details, winner criteria…"
                  />
                </FormField>

                <FormField label="Duration (hours)" hint={lockedWhileRunning ? 'Locked once running.' : 'Counted from the moment the run starts. Also drives the ladder clock: progress is whichever runs out first, budget or time.'}>
                  <Input type="number" min={0} step="any" value={form.duration_hours} disabled={lockedWhileRunning} onChange={e => set('duration_hours', e.target.value)} />
                </FormField>

                <FieldGroup
                  label="Optimization metrics"
                  hint={lockedWhileRunning
                    ? 'Locked once running — stage cuts have already ranked agents against these metrics.'
                    : <>The metric keys agents must emit. The <strong>★ primary metric</strong> ranks the leaderboard — click ☆ on another to promote it. The ↑/↓ on each chip sets its direction; click it to flip between maximize and minimize. At a stage cut an agent survives if it survives on <em>at least one</em> metric, so specialists are not eliminated by a metric they never targeted.</>}
                >
                  {lockedWhileRunning ? (
                    <div className="flex flex-wrap gap-1.5">
                      {form.metrics.map(m => (
                        <Pill key={m.key}>{m.key} {m.direction === 'maximize' ? '↑' : '↓'}</Pill>
                      ))}
                    </div>
                  ) : (
                    <MetricsEditor metrics={form.metrics} onChange={m => set('metrics', m)} />
                  )}
                </FieldGroup>

              </>
            ) : (
              <>
                {!isEdit && (
                  <FieldGroup label="Who runs the agents?" hint={RUN_BY_HINTS[form.run_by]}>
                    <div className="flex flex-wrap items-center gap-3">
                      <Segmented label="Who runs the agents?" options={RUN_BY_OPTIONS} value={form.run_by} onChange={v => set('run_by', v)} />
                      {hosted && (
                        <FormField inline label="Researchers">
                          <Input type="number" fieldSize="sm" className="w-20" min={1} step={1} value={form.researchers} onChange={e => set('researchers', Number(e.target.value))} />
                        </FormField>
                      )}
                    </div>
                  </FieldGroup>
                )}
                <div className="grid grid-cols-2 gap-3.5">
                  <FormField
                    label={`Budget (${budgetKind ? budgetUnit(budgetKind) : `${budgetUnit('accelerator')}, or ${budgetUnit('cpu')} for jobs on CPU cores`}) *`}
                    hint={lockedWhileRunning
                      ? 'Locked once running: jobs have already been admitted against this budget.'
                      : 'The one budget of the experiment. An hour on an accelerator spends what that accelerator type is priced at, and an hour on a CPU core spends 1. Memory and storage are capped per job and spend nothing.'}
                  >
                    <Input type="number" min={1} step={0.5} value={form.budget_accelerator_hours} disabled={lockedWhileRunning} onChange={e => set('budget_accelerator_hours', e.target.value)} />
                  </FormField>
                  <FormField label="Max researchers" hint={lockedWhileRunning ? 'Locked once running: the agents are fixed for the whole run.' : 'The most researchers that can join. Nobody joins or leaves once the experiment has started.'}>
                    <Input type="number" min={1} max={500} step={1} value={maxAgents} disabled={lockedWhileRunning} onChange={e => { setMaxAgentsStated(true); set('max_agents', e.target.value) }} />
                  </FormField>
                </div>

                <FieldGroup
                  label={isEdit ? 'Stage ladder (fixed at creation)' : 'Stage ladder'}
                  hint={isEdit
                    ? <>The ladder is fixed when the experiment is created and cannot be changed afterwards — agents plan around published boundaries, so moving them mid-run would invalidate the competition. Create a new experiment to use a different ladder.</>
                    : <>Each stage ends by cutting the worst-performing share of agents (their jobs stop, unspent budget redistributes) — lengths must total 100%, the final stage evicts 0%, and cuts skip entirely at ≤4 agents. <strong>Max job h</strong> caps run length per stage (empty = no limit) and is enforced.</>}
                >
                  {isEdit ? (
                    <div className="flex flex-wrap gap-1.5">
                      {form.stages.map((s, i) => (
                        <Pill key={i}>
                          {i + 1}: {s.length_pct}% {Number(s.evict_pct) > 0 ? `→ cut ${s.evict_pct}%` : '→ no cut'}{s.max_job_hours ? ` · ≤${s.max_job_hours}h/job` : ''}
                        </Pill>
                      ))}
                    </div>
                  ) : (
                    <StagesEditor stages={form.stages} onChange={s => set('stages', s)} />
                  )}
                </FieldGroup>

                <Disclosure label="More settings">
                  <div className="flex flex-col gap-4">
                    {!isEdit && (
                      <FormField
                        label="Agent image"
                        hint="The container image hosted agents and the coordinator run. Leave it empty for the platform's own agent image. A custom image must be built FROM that one, so the agent software is in it. Fixed once created."
                      >
                        <Input mono value={form.agent_image} onChange={e => set('agent_image', e.target.value)} placeholder="registry.example.com/team/agent:tag" />
                      </FormField>
                    )}
                    {!hosted && (
                      <FormField
                        label="Starts at"
                        hint={lockedWhileRunning ? 'Locked once running.' : 'Empty, the platform starts it once its baseline is declared. With a time, it starts no earlier than then. From the start no more agents join and the budget is divided among the ones that have.'}
                      >
                        <Input type="datetime-local" value={form.starts_at} disabled={lockedWhileRunning} onChange={e => set('starts_at', e.target.value)} />
                      </FormField>
                    )}
                    <FormField
                      label="Max concurrent accelerators"
                      hint={lockedWhileRunning
                        ? 'Locked once running.'
                        : "Accelerators this experiment may hold in flight (submitted + running) at once, across all agents' jobs. Empty is the platform's default."}
                    >
                      <Input type="number" min={1} step={1} placeholder="platform default" value={form.max_concurrent_accelerators} disabled={lockedWhileRunning} onChange={e => set('max_concurrent_accelerators', e.target.value)} />
                    </FormField>
                    <FormField
                      label="Report interval (seconds)"
                      hint={lockedWhileRunning
                        ? 'Locked once running.'
                        : "How often agents are expected to push metrics. Empty is the platform's default. A job that reports nothing for 3× this interval is treated as dead and evicted."}
                    >
                      <Input type="number" min={1} step={1} placeholder="platform default" value={form.report_interval_seconds} disabled={lockedWhileRunning} onChange={e => set('report_interval_seconds', e.target.value)} />
                    </FormField>
                  </div>
                </Disclosure>

              </>
            )}

            {error && <ErrorMessage>{error}</ErrorMessage>}

            <div className="mt-1 flex justify-end gap-2">
              {onCancel && <Button type="button" onClick={onCancel}>Cancel</Button>}
              {step === 1 ? (
                <Button type="button" variant="primary" onClick={() => { setError(null); setStep(2) }}>Next: budget and rules</Button>
              ) : (
                <>
                  <Button type="button" onClick={() => { setError(null); setStep(1) }}>Back</Button>
                  {!isEdit && needsGitHub ? <ConnectGitHubStep /> : (
                    <Button type="submit" variant="primary" disabled={submitting || (!isEdit && !!ladderError)}>
                      {submitting ? (hosted ? 'Creating and starting…' : 'Saving…') : isEdit ? 'Save changes' : hosted ? 'Create and run' : 'Create experiment'}
                    </Button>
                  )}
                </>
              )}
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

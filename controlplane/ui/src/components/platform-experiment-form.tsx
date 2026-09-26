'use client'

import React, { useState } from 'react'
import { clsx } from 'clsx'
import { createPlatformExperiment, updatePlatformExperiment } from '@/lib/api'
import type { Stage } from '@/lib/api'
import { isZeroDate } from '@/lib/format'
import { createAndRun } from '@/lib/run-experiment'
import type { RunFailure } from '@/lib/run-experiment'
import { useTenant } from '@/lib/tenant'
import { COMMON_ML_METRICS, PlatformExperimentStatus } from '@/types'
import type { MetricDefinition, PlatformExperiment, SubmitterPolicy } from '@/types'
import { RepositorySelect } from '@/components/repository-select'
import { Button } from '@/components/ui/button'
import { Dialog } from '@/components/ui/dialog'
import { Disclosure } from '@/components/ui/disclosure'
import { FieldGroup, FormField } from '@/components/ui/form-field'
import { Input, Textarea } from '@/components/ui/input'
import { Select } from '@/components/ui/select'
import { Segmented, Tabs } from '@/components/ui/tabs'
import { Eyebrow, Pill } from '@/components/ui/text'
import { ErrorMessage, InlineError } from '@/components/ui/status-message'

function localDatetime(offsetDays: number): string {
  const d = new Date()
  d.setDate(d.getDate() + offsetDays)
  d.setSeconds(0, 0)
  return d.toISOString().slice(0, 16)
}

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
  starts_at: string
  ends_at: string
  metrics: MetricDefinition[]
  report_interval_seconds: number
  stages: StageDraft[]
  hypothesis_submit_policy: SubmitterPolicy
  job_submit_policy: SubmitterPolicy
  repository: string
  agent_image: string
  run_by: RunBy
  researchers: number
}

// Who runs the agents of a new experiment. 'platform' creates it with a hosted coordinator, adds
// hosted agents and starts it; 'operator' creates it and leaves the rest to its page.
type RunBy = 'platform' | 'operator'

const RUN_BY_OPTIONS: Array<{ value: RunBy; label: string }> = [
  { value: 'operator', label: 'I run my own agents' },
  { value: 'platform', label: 'Hosted agents' },
]

const RUN_BY_HINTS: Record<RunBy, string> = {
  operator: 'The experiment is created and nothing else. You add agents on its page, run by you or by the platform, and it starts at its start time or when you start it.',
  platform: 'The platform runs a coordinator and this many researcher agents on machines of its own, and the experiment starts as soon as they have joined. How many it runs for one experiment is the platform\'s limit, and it says so when it is passed.',
}

// What the API asks of an experiment whose agents and coordinator the platform runs.
const HOSTED_NEEDS_REPOSITORY = 'Hosted agents and the coordinator work in the experiment\'s repository. Choose one under Essentials first.'

const POLICY_OPTIONS: Array<{ value: SubmitterPolicy; label: string }> = [
  { value: 'mixed', label: 'Anyone' },
  { value: 'human_only', label: 'Humans' },
  { value: 'agent_only', label: 'Agents' },
]

const POLICY_HINTS: Record<'hypothesis' | 'job', Record<SubmitterPolicy, string>> = {
  hypothesis: {
    mixed: 'Both humans and agents may propose hypotheses.',
    human_only: 'Only human participants may propose hypotheses. An agent that tries is rejected with a clear error naming this policy.',
    agent_only: 'Only agents may propose hypotheses — human-submitted ideas are rejected.',
  },
  job: {
    mixed: 'Both humans and agents may submit jobs.',
    human_only: 'Only human participants may submit jobs. An agent that fires one off — even accidentally, e.g. a leftover automation — is rejected with an error naming this policy, before anything runs or is billed.',
    agent_only: 'Only agents may submit jobs — a human cannot run one directly.',
  },
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
  /**
   * Told the experiment the API answered with, once it is saved. `run` is set when the form went
   * on to add hosted agents and start it, with what stopped that when something did.
   */
  onSaved: (saved: PlatformExperiment, run?: { failure?: RunFailure }) => void
  /** Offers Cancel. A form that is the page's own content has nothing to go back to and passes none. */
  onCancel?: () => void
}

/**
 * The one form a platform experiment is created and edited with, in two steps. It checks only
 * what it needs to build the request; every other rule is the API's, whose refusal it shows.
 */
export function PlatformExperimentForm({ initial, onSaved, onCancel }: FormProps) {
  const { project } = useTenant()
  const isEdit = !!initial
  // Once running, admission/stage decisions have already been made against budget, max_agents,
  // metrics, the schedule and the report interval — the backend rejects changes to those (see
  // Update in platform_experiments_lifecycle.go), so lock them here too instead of round-tripping
  // a 400. Name/description stay editable: purely informational, safe to amend mid-run.
  const lockedWhileRunning = isEdit && initial?.status === PlatformExperimentStatus.RUNNING
  const [form, setForm] = useState<FormState>({
    name: initial?.name ?? '',
    description: initial?.description ?? '',
    budget_accelerator_hours: initial?.budget_accelerator_hours ?? 1000,
    max_agents: initial?.max_agents ?? 20,
    max_concurrent_accelerators: initial?.max_concurrent_accelerators != null ? String(initial.max_concurrent_accelerators) : '',
    starts_at: !isZeroDate(initial?.starts_at) ? new Date(initial!.starts_at!).toISOString().slice(0, 16) : localDatetime(1),
    ends_at: !isZeroDate(initial?.ends_at) ? new Date(initial!.ends_at!).toISOString().slice(0, 16) : localDatetime(8),
    metrics: initial?.metrics ?? [],
    report_interval_seconds: initial?.report_interval_seconds ?? 30,
    stages: initial?.stages?.length ? toDrafts(initial.stages) : DEFAULT_STAGES,
    hypothesis_submit_policy: initial?.hypothesis_submit_policy ?? 'mixed',
    job_submit_policy: initial?.job_submit_policy ?? 'mixed',
    repository: initial?.repository ?? '',
    agent_image: initial?.agent_image ?? '',
    run_by: 'operator',
    researchers: 2,
  })
  const [step, setStep] = useState<1 | 2>(1)
  const [submitting, setSubmitting] = useState(false)
  const [error, setError] = useState<string | null>(null)

  function set(field: keyof FormState, value: string | number | MetricDefinition[] | StageDraft[] | SubmitterPolicy | RunBy) {
    setForm(f => ({ ...f, [field]: value }))
  }

  const ladderError = stagesError(form.stages)
  const hosted = !isEdit && form.run_by === 'platform'
  const hostedError = hosted && !form.repository ? HOSTED_NEEDS_REPOSITORY : null

  function step1Error(): string | null {
    if (!form.name.trim()) return 'Name is required.'
    // An experiment the form starts itself has no start time of its own to compare with.
    if (hosted) return new Date(form.ends_at) <= new Date() ? 'Ends At must be in the future.' : null
    if (new Date(form.ends_at) <= new Date(form.starts_at)) return 'Ends At must be after Starts At.'
    return null
  }

  // Lives on step 2 now (moved alongside Max participants), so it no longer gates the step 1→2
  // transition — only the final submit, same as the ladder.
  function step2BudgetError(): string | null {
    return Number(form.budget_accelerator_hours) <= 0 ? 'Total compute budget must be greater than 0.' : null
  }

  async function handleSubmit(e: React.FormEvent) {
    e.preventDefault()
    // Both steps are validated here, not just the visible one — a bad ladder on step 2 must not
    // be submittable just because step 1 passed. Tab navigation itself is never gated — only
    // submit is, so the user is free to jump between tabs while filling the form out of order.
    const err = step1Error() ?? step2BudgetError() ?? (isEdit ? null : ladderError) ?? hostedError
    if (err) { setError(err); return }
    setSubmitting(true)
    setError(null)
    try {
      const payload = {
        name: form.name.trim(),
        description: form.description.trim(),
        budget_accelerator_hours: Number(form.budget_accelerator_hours),
        max_agents: Number(form.max_agents),
        ...(form.max_concurrent_accelerators.trim() !== '' ? { max_concurrent_accelerators: Number(form.max_concurrent_accelerators) } : {}),
        metrics: form.metrics,
        report_interval_seconds: Number(form.report_interval_seconds),
        hypothesis_submit_policy: form.hypothesis_submit_policy,
        job_submit_policy: form.job_submit_policy,
        // Omitted when editing: the backend fixes the ladder at creation and silently ignores
        // it on update, so sending it would imply an edit that never happens.
        ...(isEdit ? {} : { stages: toStages(form.stages) }),
        // The repository and the agent image are fixed at creation too.
        ...(!isEdit && form.repository ? { repository: form.repository } : {}),
        ...(!isEdit && form.agent_image.trim() ? { agent_image: form.agent_image.trim() } : {}),
        starts_at: new Date(form.starts_at).toISOString(),
        ends_at: new Date(form.ends_at).toISOString(),
      }
      if (hosted) {
        const { experiment, failure } = await createAndRun(project.id, payload, Number(form.researchers))
        onSaved(experiment, { failure })
      } else {
        onSaved(isEdit && initial
          ? await updatePlatformExperiment(initial.id, payload)
          : await createPlatformExperiment(project.id, payload))
      }
    } catch (err: unknown) {
      setError(err instanceof Error ? err.message : String(err))
    } finally {
      setSubmitting(false)
    }
  }

  return (
    <>
        <div className="mb-4">
          <Tabs
            label="Steps"
            tabs={[{ id: '1', label: '1. Essentials' }, { id: '2', label: '2. Competition settings' }]}
            value={String(step) as '1' | '2'}
            onChange={id => { setError(null); setStep(id === '1' ? 1 : 2) }}
          />
        </div>

        <form onSubmit={handleSubmit}>
          <div className="flex flex-col gap-4">
            {step === 1 ? (
              <>
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

                <div className="grid grid-cols-2 gap-3.5">
                  <FormField
                    label="Starts at"
                    hint={lockedWhileRunning ? 'Locked once running.'
                      : hosted ? 'Not used: with hosted agents it starts as soon as they have joined.'
                        : 'Signups close and quota is allocated at this moment.'}
                  >
                    <Input type="datetime-local" value={form.starts_at} disabled={lockedWhileRunning || hosted} onChange={e => set('starts_at', e.target.value)} />
                  </FormField>
                  <FormField label="Ends at" hint={lockedWhileRunning ? 'Locked once running.' : 'Also drives the ladder clock: progress is whichever runs out first, budget or time.'}>
                    <Input type="datetime-local" value={form.ends_at} disabled={lockedWhileRunning} onChange={e => set('ends_at', e.target.value)} />
                  </FormField>
                </div>

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
                      hint="The code this experiment works on: agents clone it and push their branches to it, and jobs check it out. Fixed once created."
                    >
                      <RepositorySelect value={form.repository} onChange={r => set('repository', r)} />
                    </FormField>
                    <Disclosure label="Advanced">
                      <FormField
                        label="Agent image"
                        hint="The container image hosted agents and the coordinator run. Leave it empty for the platform's own agent image. A custom image must be built FROM that one, so the harness is in it. Fixed once created."
                      >
                        <Input mono value={form.agent_image} onChange={e => set('agent_image', e.target.value)} placeholder="registry.example.com/team/agent:tag" />
                      </FormField>
                    </Disclosure>
                  </>
                )}
              </>
            ) : (
              <>
                <div className="grid grid-cols-2 gap-3.5">
                  <FormField
                    label="Accelerator budget — AccH (GPU-hours) *"
                    hint={lockedWhileRunning
                      ? 'Locked once running — admission has already committed usage against this budget.'
                      : <>Normalized H100-equivalent hours — the <strong>only</strong> budget (H100 1.0, A100 0.375, L40 0.25/hr). CPU, RAM and storage are capped per job instead, not budgeted.</>}
                  >
                    <Input type="number" min={1} step={0.5} value={form.budget_accelerator_hours} disabled={lockedWhileRunning} onChange={e => set('budget_accelerator_hours', e.target.value)} />
                  </FormField>
                  <FormField label="Max participants" hint={lockedWhileRunning ? 'Locked once running — the roster is fixed for the run.' : 'Signup cap. Signups close when the experiment starts, and the roster is then fixed for the whole run.'}>
                    <Input type="number" min={1} max={500} step={1} value={form.max_agents} disabled={lockedWhileRunning} onChange={e => set('max_agents', e.target.value)} />
                  </FormField>
                </div>

                <FormField
                  label="Max concurrent accelerators"
                  hint={lockedWhileRunning
                    ? 'Locked once running.'
                    : "Accelerators this experiment may hold in flight (submitted + running) at once, across all agents' jobs. Empty = platform default. Bounds spend rate now that a job can trigger a cluster scale-up on demand."}
                >
                  <Input type="number" min={1} step={1} placeholder="platform default" value={form.max_concurrent_accelerators} disabled={lockedWhileRunning} onChange={e => set('max_concurrent_accelerators', e.target.value)} />
                </FormField>

                <FieldGroup
                  label={isEdit ? 'Stage ladder (fixed at creation)' : 'Stage ladder'}
                  hint={isEdit
                    ? <>The ladder is fixed when the experiment is created and cannot be changed afterwards — agents plan around published boundaries, so moving them mid-run would invalidate the competition. Create a new platform experiment to use a different ladder.</>
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

                <FormField
                  label="Report interval (seconds)"
                  hint={lockedWhileRunning
                    ? 'Locked once running.'
                    : "How often agents are expected to push metrics. A job that reports nothing for 3× this interval is treated as dead and evicted, so set it to match your real training loop's logging cadence — too low and healthy jobs get killed during a reschedule."}
                >
                  <Input type="number" min={1} step={1} value={form.report_interval_seconds} disabled={lockedWhileRunning} onChange={e => set('report_interval_seconds', e.target.value)} />
                </FormField>

                <div className="grid grid-cols-2 gap-3.5">
                  <FieldGroup label="Who can submit hypotheses?" hint={POLICY_HINTS.hypothesis[form.hypothesis_submit_policy]}>
                    <Segmented
                      label="Who can submit hypotheses?"
                      options={POLICY_OPTIONS}
                      value={form.hypothesis_submit_policy}
                      disabled={lockedWhileRunning}
                      onChange={v => set('hypothesis_submit_policy', v)}
                    />
                  </FieldGroup>
                  <FieldGroup label="Who can submit jobs?" hint={POLICY_HINTS.job[form.job_submit_policy]}>
                    <Segmented
                      label="Who can submit jobs?"
                      options={POLICY_OPTIONS}
                      value={form.job_submit_policy}
                      disabled={lockedWhileRunning}
                      onChange={v => set('job_submit_policy', v)}
                    />
                  </FieldGroup>
                </div>
                {lockedWhileRunning && (
                  <p className="-mt-2 text-2xs leading-normal text-subtle">
                    Locked once running — who may submit has already shaped who signed up.
                  </p>
                )}

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
                {hostedError && <InlineError>{hostedError}</InlineError>}
              </>
            )}

            {error && <ErrorMessage>{error}</ErrorMessage>}

            <div className="mt-1 flex justify-end gap-2">
              {onCancel && <Button type="button" onClick={onCancel}>Cancel</Button>}
              {step === 1 ? (
                <Button type="button" variant="primary" onClick={() => { setError(null); setStep(2) }}>Next: competition settings</Button>
              ) : (
                <>
                  <Button type="button" onClick={() => { setError(null); setStep(1) }}>Back</Button>
                  <Button type="submit" variant="primary" disabled={submitting || (!isEdit && !!ladderError) || !!hostedError}>
                    {submitting ? (hosted ? 'Creating and starting…' : 'Saving…') : isEdit ? 'Save Changes' : hosted ? 'Create and run' : 'Create Experiment'}
                  </Button>
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
    <Dialog open onClose={onClose} title={initial ? 'Edit Platform Experiment' : 'New Platform Experiment'}>
      <PlatformExperimentForm initial={initial} onCancel={onClose} onSaved={(saved, run) => { onSaved(saved, run); onClose() }} />
    </Dialog>
  )
}

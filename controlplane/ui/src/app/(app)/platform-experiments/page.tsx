'use client'

import React, { Suspense, useState } from 'react'
import useSWR from 'swr'
import { useProjectRouter, useQueryParam } from '@/lib/query-state'
import { useTenant } from '@/lib/tenant'
import { clsx } from 'clsx'
import {
  fetchPlatformExperimentsPage,
  fetchPlatformExperimentQuotas,
  fetchExperimentStats,
  fetchHypothesisStats,
  HYPOTHESIS_STATUSES,
  createPlatformExperiment,
  updatePlatformExperiment,
} from '@/lib/api'
import type { PlatformExperiment, AgentQuota, MetricDefinition, SubmitterPolicy } from '@/types'
import { PlatformExperimentStatus } from '@/types'
import type { Stage } from '@/lib/api'
import { quotaRemainingAccH } from '@/lib/quota'
import { COMMON_ML_METRICS } from '@/types'
import { hypothesisActivity } from '@/lib/hypothesis-progress'
import { formatDate, isZeroDate } from '@/lib/format'
import { PageHeader } from '@/components/ui/page-header'
import { Card, CardHeader, CardContent } from '@/components/ui/card'
import { Badge } from '@/components/ui/badge'
import { Button, Chip } from '@/components/ui/button'
import { Dialog } from '@/components/ui/dialog'
import { FieldGroup, FormField } from '@/components/ui/form-field'
import { Input, Textarea } from '@/components/ui/input'
import { Select } from '@/components/ui/select'
import { Table } from '@/components/ui/table'
import { Segmented, Tabs } from '@/components/ui/tabs'
import { Eyebrow, Pill } from '@/components/ui/text'
import { Loading, ErrorMessage } from '@/components/ui/status-message'
import { Pagination } from '@/components/ui/pagination'
import { CollapsibleDescription } from '@/components/ui/collapsible-description'
import { statusColor } from '@/lib/colors'
import { formatAccH } from '@/lib/format'

function StatusBadge({ status }: { status: string }) {
  return <Badge status={status}>{status}</Badge>
}

// The primary metric ranks the leaderboard, so its pill carries the accent ring.
function MetricChip({ m, primary }: { m: MetricDefinition; primary?: boolean }) {
  return (
    <Pill tone={m.direction === 'maximize' ? 'success' : 'warning'} active={primary} title={primary ? 'Primary metric — used for ranking' : undefined}>
      {primary && <span className="opacity-80">★</span>}
      {m.key} {m.direction === 'maximize' ? '↑' : '↓'}
    </Pill>
  )
}

function ExperimentCard({
  pe,
  onEdit,
}: {
  pe: PlatformExperiment
  onEdit: (pe: PlatformExperiment) => void
}) {
  const router = useProjectRouter()
  const [expanded, setExpanded] = useState(false)

  // Fetched unconditionally (not gated on `expanded`) — the Budget Used stat tile above the
  // expand toggle needs this data too, otherwise it shows 0 until the card is expanded once.
  const { data: quotas } = useSWR<AgentQuota[]>(
    ['pe-quotas', pe.id],
    () => fetchPlatformExperimentQuotas(pe.id),
    { refreshInterval: 10_000 },
  )

  const { data: verdicts } = useSWR(
    ['pe-hypothesis-stats', pe.id],
    () => fetchHypothesisStats(pe.id),
    { refreshInterval: 15_000 },
  )
  const { data: hypothesisStats } = useSWR(
    ['pe-stats-by-hypothesis', pe.id],
    () => fetchExperimentStats({ project_id: pe.project_id, platform_experiment_id: pe.id, group_by: 'hypothesis' }),
    { refreshInterval: 15_000 },
  )
  const hypothesisCount = verdicts?.total ?? 0
  const activity = hypothesisActivity(hypothesisCount, hypothesisStats?.groups ?? {})

  const totalUsed = (quotas ?? []).reduce((s, q) => s + q.used_guaranteed_acch + q.used_burst_acch, 0)
  const daysLeft = !isZeroDate(pe.ends_at)
    ? Math.ceil((new Date(pe.ends_at!).getTime() - Date.now()) / 86_400_000)
    : null

  return (
    <Card className="cursor-pointer">
      <CardHeader
        className="justify-between"
        onClick={() => router.push(`/platform-experiments/${pe.id}`)}
        tabIndex={0}
        role="link"
        onKeyDown={e => { if (e.key === 'Enter') router.push(`/platform-experiments/${pe.id}`) }}
      >
        <div className="flex items-center gap-2.5">
          <StatusBadge status={pe.status} />
          <span className="text-base font-bold">{pe.name}</span>
        </div>
        <div className="flex items-center gap-3 font-normal">
          {!isZeroDate(pe.starts_at) && (
            <span className="text-xs text-subtle">{formatDate(pe.starts_at)} – {formatDate(pe.ends_at)}</span>
          )}
          {daysLeft != null && daysLeft > 0 && (
            <span className={clsx('text-2xs', daysLeft < 2 ? 'text-danger' : 'text-muted')}>{daysLeft}d remaining</span>
          )}
          <Button size="sm" onClick={e => { e.stopPropagation(); onEdit(pe) }}>Edit</Button>
          <span className="text-xs text-muted" onClick={e => { e.stopPropagation(); setExpanded(v => !v) }}>{expanded ? '▲' : '▼'}</span>
        </div>
      </CardHeader>

      <CardContent onClick={() => router.push(`/platform-experiments/${pe.id}`)}>
        {pe.description && <CollapsibleDescription text={pe.description} className="mb-3" />}

        {pe.metrics && pe.metrics.length > 0 && (
          <div className="mb-3.5 flex flex-wrap gap-1.5">
            {pe.metrics.map((m, i) => <MetricChip key={m.key} m={m} primary={i === 0} />)}
          </div>
        )}

        {pe.stages && pe.stages.length > 0 && (
          <div className="mb-3.5 flex flex-wrap items-center gap-1.5">
            <Eyebrow className="mr-0.5">Ladder</Eyebrow>
            {pe.stages.map((s, i) => {
              const active = i + 1 === pe.current_stage
              return (
                <Pill
                  key={i}
                  tone={active ? 'accent' : undefined}
                  active={active}
                  title={`Stage ${i + 1}: spans ${s.length_pct}% of the run${s.evict_pct > 0 ? `, then cuts ${s.evict_pct}% of survivors` : ', no cut'}${s.max_job_hours ? `; no job may run longer than ${s.max_job_hours}h` : '; no job length limit'}`}
                >
                  {s.length_pct}% {s.evict_pct > 0 ? `→ cut ${s.evict_pct}%` : '→ no cut'}{s.max_job_hours ? ` · ≤${s.max_job_hours}h/job` : ''}
                </Pill>
              )
            })}
          </div>
        )}

        <div className="mb-3.5 grid grid-cols-4 gap-4">
          <div>
            <Eyebrow className="block">Budget</Eyebrow>
            <div className="font-mono text-xl font-bold">{pe.budget_accelerator_hours} <span className="text-xs text-muted">AccH</span></div>
          </div>
          <div>
            <Eyebrow className="block">Agents</Eyebrow>
            <div className="font-mono text-xl font-bold">{pe.signup_count} <span className="text-xs text-muted">/ {pe.max_agents}</span></div>
          </div>
          <div>
            <Eyebrow className="block">Budget Used</Eyebrow>
            <div className={clsx('font-mono text-xl font-bold', totalUsed > 0 && 'text-accent')}>
              {formatAccH(totalUsed)} <span className="text-xs text-muted">AccH</span>
            </div>
          </div>
          <div>
            <Eyebrow className="block">Hypotheses</Eyebrow>
            <div className="flex flex-wrap items-baseline gap-2.5">
              {HYPOTHESIS_STATUSES.map(v => (
                <span key={v} className="font-mono text-sm" style={{ color: statusColor(v) }}>
                  {verdicts?.by_status[v] ?? 0} <span className="text-2xs text-muted">{v}</span>
                </span>
              ))}
            </div>
            {activity && <div className="text-2xs text-muted">{activity}</div>}
          </div>
        </div>

        {expanded && (
          <div onClick={e => e.stopPropagation()}>
            <div className="border-t pt-3.5">
              <Eyebrow className="mb-2.5 block">
                Agent Quotas — Guaranteed &amp; Burst (AccH)
              </Eyebrow>
              {!quotas ? (
                <p className="text-xs text-subtle">Loading quotas…</p>
              ) : quotas.length === 0 ? (
                <p className="text-xs text-subtle">No quotas allocated yet.</p>
              ) : (
                <Table>
                  <thead>
                    <tr>
                      <th>Agent</th>
                      <th>Guaranteed</th>
                      <th>Burst</th>
                      <th>Remaining</th>
                    </tr>
                  </thead>
                  <tbody>
                    {quotas.map(q => {
                      const remaining = quotaRemainingAccH(q)
                      return (
                        <tr key={q.id}>
                          <td className="font-mono text-xs font-semibold">{q.agent_id}</td>
                          <td className="font-mono text-2xs">{formatAccH(q.used_guaranteed_acch)} / {formatAccH(q.guaranteed_accelerator_hours)} AccH</td>
                          <td className="font-mono text-2xs">{formatAccH(q.used_burst_acch)} / {formatAccH(q.burst_accelerator_hours)} AccH</td>
                          <td className={clsx('font-mono text-2xs', remaining > 0 ? 'text-success' : 'text-danger')}>{formatAccH(remaining)} AccH</td>
                        </tr>
                      )
                    })}
                  </tbody>
                </Table>
              )}
            </div>
          </div>
        )}
      </CardContent>
    </Card>
  )
}

// ---- Experiment Modal (create / edit) ----

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
}

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

function ExperimentModal({
  initial,
  onClose,
  onSaved,
}: {
  initial?: PlatformExperiment | null
  onClose: () => void
  onSaved: () => void
}) {
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
  })
  const [step, setStep] = useState<1 | 2>(1)
  const [submitting, setSubmitting] = useState(false)
  const [error, setError] = useState<string | null>(null)

  function set(field: keyof FormState, value: string | number | MetricDefinition[] | StageDraft[] | SubmitterPolicy) {
    setForm(f => ({ ...f, [field]: value }))
  }

  const ladderError = stagesError(form.stages)

  function step1Error(): string | null {
    if (!form.name.trim()) return 'Name is required.'
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
    const err = step1Error() ?? step2BudgetError() ?? (isEdit ? null : ladderError)
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
        starts_at: new Date(form.starts_at).toISOString(),
        ends_at: new Date(form.ends_at).toISOString(),
      }
      if (isEdit && initial) {
        await updatePlatformExperiment(initial.id, payload)
      } else {
        await createPlatformExperiment(project.id, payload)
      }
      onSaved()
      onClose()
    } catch (err: unknown) {
      setError(err instanceof Error ? err.message : String(err))
    } finally {
      setSubmitting(false)
    }
  }

  return (
    <Dialog open onClose={onClose} title={isEdit ? 'Edit Platform Experiment' : 'New Platform Experiment'}>
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
                  <FormField label="Starts at" hint={lockedWhileRunning ? 'Locked once running.' : 'Signups close and quota is allocated at this moment.'}>
                    <Input type="datetime-local" value={form.starts_at} disabled={lockedWhileRunning} onChange={e => set('starts_at', e.target.value)} />
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
              </>
            )}

            {error && <ErrorMessage>{error}</ErrorMessage>}

            <div className="mt-1 flex justify-end gap-2">
              <Button type="button" onClick={onClose}>Cancel</Button>
              {step === 1 ? (
                <Button type="button" variant="primary" onClick={() => { setError(null); setStep(2) }}>Next: competition settings</Button>
              ) : (
                <>
                  <Button type="button" onClick={() => { setError(null); setStep(1) }}>Back</Button>
                  <Button type="submit" variant="primary" disabled={submitting || (!isEdit && !!ladderError)}>
                    {submitting ? 'Saving…' : isEdit ? 'Save Changes' : 'Create Experiment'}
                  </Button>
                </>
              )}
            </div>
          </div>
        </form>
    </Dialog>
  )
}

// ---- Page ----

const PAGE_SIZE = 10

const SORT_OPTIONS = [
  { value: '-starts_at', label: 'Newest first' },
  { value: 'starts_at', label: 'Oldest first' },
  { value: 'name', label: 'Name (A–Z)' },
  { value: '-name', label: 'Name (Z–A)' },
]

export default function PlatformExperimentsPage() {
  return (
    <Suspense fallback={<Loading />}>
      <PlatformExperimentsPageContent />
    </Suspense>
  )
}

function PlatformExperimentsPageContent() {
  const { org, project } = useTenant()
  const [statusFilter, setStatusFilter] = useQueryParam('status')
  const [search, setSearch] = useQueryParam('q')
  const [sort, setSort] = useQueryParam('sort', '-starts_at')
  const [page, setPage] = useState(0)
  const [modal, setModal] = useState<{ open: boolean; editing?: PlatformExperiment | null }>({ open: false })

  const { data, error, isLoading, mutate } = useSWR(
    ['platform-experiments', project.id, statusFilter, search, sort, page],
    () => fetchPlatformExperimentsPage({
      project_id: project.id,
      status: statusFilter || undefined,
      q: search || undefined,
      sort,
      limit: PAGE_SIZE,
      offset: page * PAGE_SIZE,
    }),
    { refreshInterval: 10_000, keepPreviousData: true },
  )

  const experiments = data?.items ?? []
  const total = data?.total ?? 0
  const statuses = ['', 'open', 'running', 'draft', 'closed']

  return (
    <div>
      {modal.open && (
        <ExperimentModal
          initial={modal.editing}
          onClose={() => setModal({ open: false })}
          onSaved={() => mutate()}
        />
      )}

      <PageHeader
        title="Platform Experiments"
        description="Operator-defined compute envelopes. Agents sign up and compete for AccH quota within each experiment."
        actions={<Button size="sm" variant="primary" onClick={() => setModal({ open: true, editing: null })}>+ New Experiment</Button>}
      />

      <div className="mb-4 flex flex-wrap items-center gap-3">
        <div className="flex flex-wrap gap-1.5">
          {statuses.map(s => (
            <Chip key={s} active={statusFilter === s} onClick={() => { setStatusFilter(s); setPage(0) }}>
              {s || 'All'}
            </Chip>
          ))}
        </div>

        <Input
          fieldSize="sm"
          mono
          className="min-w-[220px]"
          value={search}
          onChange={e => { setSearch(e.target.value); setPage(0) }}
          placeholder="Search name or description…"
        />

        <Select fieldSize="sm" aria-label="Sort" prefix="Sort" value={sort} onChange={v => { setSort(v); setPage(0) }} options={SORT_OPTIONS} />
      </div>

      {isLoading && <Loading />}
      {error && <ErrorMessage>Could not load the platform experiments of {project.name}: {error.message}</ErrorMessage>}

      {data && experiments.length === 0 && (statusFilter || search ? (
        <Card>
          <CardContent>
            <div className="py-2 text-center text-xs text-subtle">No platform experiments in this project match{search ? ` "${search}"` : ' this filter'}.</div>
          </CardContent>
        </Card>
      ) : (
        <Card>
          <CardContent>
            <div className="py-4 text-center">
              <div className="mb-1.5 text-lg font-bold">{org.name} / {project.name} has no platform experiments yet</div>
              <p className="mx-auto mb-3.5 max-w-[520px] text-sm leading-relaxed text-muted">
                A platform experiment sets the budget, the metrics and the time window agents work
                within. Create one, then open it to add agents.
              </p>
              <Button variant="primary" onClick={() => setModal({ open: true, editing: null })}>
                Create the first platform experiment
              </Button>
            </div>
          </CardContent>
        </Card>
      ))}

      {experiments.map(pe => (
        <ExperimentCard
          key={pe.id}
          pe={pe}
          onEdit={pe => setModal({ open: true, editing: pe })}
        />
      ))}

      <Pagination page={page} pageSize={PAGE_SIZE} total={total} onPageChange={setPage} />
    </div>
  )
}

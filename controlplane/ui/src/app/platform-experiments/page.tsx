'use client'

import React, { useState } from 'react'
import { useRouter } from 'next/navigation'
import useSWR from 'swr'
import { Listbox, ListboxButton, ListboxOption, ListboxOptions } from '@headlessui/react'
import {
  fetchPlatformExperimentsPage,
  fetchPlatformExperimentQuotas,
  fetchExperimentsPage,
  fetchHypothesesPage,
  MAX_LIST_PAGE_SIZE,
  createPlatformExperiment,
  updatePlatformExperiment,
} from '@/lib/api'
import type { PlatformExperiment, AgentQuota, MetricDefinition, Experiment, Hypothesis, SubmitterPolicy } from '@/types'
import { PlatformExperimentStatus } from '@/types'
import type { Stage } from '@/lib/api'
import { quotaRemainingAccH } from '@/lib/quota'
import { COMMON_ML_METRICS } from '@/types'
import { hypothesisProgressCounts } from '@/lib/hypothesis-progress'
import { formatDate, isZeroDate } from '@/lib/format'
import { PageHeader } from '@/components/ui/page-header'
import { Pod, PodHeader, PodContent } from '@/components/ui/pod'
import { Badge } from '@/components/ui/badge'
import { Button, Chip } from '@/components/ui/button'
import { Loading, ErrorMessage } from '@/components/ui/status-message'
import { Pagination } from '@/components/ui/pagination'
import { CollapsibleDescription } from '@/components/ui/collapsible-description'
import { semantic } from '@/lib/colors'
import { formatAccH } from '@/lib/format'

function StatusBadge({ status }: { status: string }) {
  return <Badge status={status}>{status}</Badge>
}

function MetricChip({ m, primary }: { m: MetricDefinition; primary?: boolean }) {
  return (
    <span
      className="mono"
      style={{
        fontSize: 11, padding: '3px 9px', borderRadius: 999,
        display: 'inline-flex', alignItems: 'center', gap: 4,
        background: m.direction === 'maximize' ? 'rgba(74, 222, 128, 0.12)' : 'rgba(251, 191, 36, 0.12)',
        border: primary
          ? `1px solid ${semantic.accent}`
          : `1px solid ${m.direction === 'maximize' ? 'rgba(74, 222, 128, 0.3)' : 'rgba(251, 191, 36, 0.3)'}`,
        color: m.direction === 'maximize' ? semantic.success : semantic.warning,
        boxShadow: primary ? `0 0 0 1px ${semantic.accent} inset` : undefined,
      }}
      title={primary ? 'Primary metric — used for ranking' : undefined}
    >
      {primary && <span style={{ opacity: 0.85, fontSize: 9 }}>★</span>}
      {m.key} {m.direction === 'maximize' ? '↑' : '↓'}
    </span>
  )
}

function ExperimentCard({
  pe,
  onEdit,
}: {
  pe: PlatformExperiment
  onEdit: (pe: PlatformExperiment) => void
}) {
  const router = useRouter()
  const [expanded, setExpanded] = useState(false)

  // Fetched unconditionally (not gated on `expanded`) — the Budget Used stat tile above the
  // expand toggle needs this data too, otherwise it shows 0 until the card is expanded once.
  const { data: quotas } = useSWR<AgentQuota[]>(
    ['pe-quotas', pe.id],
    () => fetchPlatformExperimentQuotas(pe.id),
    { refreshInterval: 10_000 },
  )

  // Progress is derived by joining this experiment's hypotheses to its jobs, which no single
  // endpoint returns — so both are read as one explicitly bounded page each, and the card says
  // so when either set is larger than the window rather than presenting a partial tally as the
  // whole picture.
  const { data: hypotheses } = useSWR(
    ['pe-hypotheses', pe.id],
    () => fetchHypothesesPage({ platform_experiment_id: pe.id, limit: MAX_LIST_PAGE_SIZE }),
    { refreshInterval: 15_000 },
  )
  const { data: jobs } = useSWR(
    ['pe-experiments', pe.id],
    () => fetchExperimentsPage({ platform_experiment_id: pe.id, limit: MAX_LIST_PAGE_SIZE }),
    { refreshInterval: 10_000 },
  )
  const progress = hypothesisProgressCounts(hypotheses?.items ?? [], jobs?.items ?? [])
  const progressPartial =
    (hypotheses?.total ?? 0) > (hypotheses?.items.length ?? 0) ||
    (jobs?.total ?? 0) > (jobs?.items.length ?? 0)

  const totalUsed = (quotas ?? []).reduce((s, q) => s + q.used_guaranteed_acch + q.used_burst_acch, 0)
  const daysLeft = !isZeroDate(pe.ends_at)
    ? Math.ceil((new Date(pe.ends_at!).getTime() - Date.now()) / 86_400_000)
    : null

  return (
    <Pod style={{ cursor: 'pointer' }}>
      <PodHeader
        style={{ justifyContent: 'space-between' }}
        onClick={() => router.push(`/platform-experiments/${pe.id}`)}
        tabIndex={0}
        role="link"
        onKeyDown={e => { if (e.key === 'Enter') router.push(`/platform-experiments/${pe.id}`) }}
      >
        <div style={{ display: 'flex', alignItems: 'center', gap: 10 }}>
          <StatusBadge status={pe.status} />
          <span style={{ fontSize: 14, fontWeight: 700 }}>{pe.name}</span>
        </div>
        <div style={{ display: 'flex', alignItems: 'center', gap: 12, fontWeight: 400 }}>
          {!isZeroDate(pe.starts_at) && (
            <span className="text-dim">{formatDate(pe.starts_at)} – {formatDate(pe.ends_at)}</span>
          )}
          {daysLeft != null && daysLeft > 0 && (
            <span style={{ fontSize: 11, color: daysLeft < 2 ? semantic.danger : 'var(--muted-fg)' }}>{daysLeft}d remaining</span>
          )}
          <Button size="sm" onClick={e => { e.stopPropagation(); onEdit(pe) }}>Edit</Button>
          <span className="text-muted" style={{ fontSize: 12 }} onClick={e => { e.stopPropagation(); setExpanded(v => !v) }}>{expanded ? '▲' : '▼'}</span>
        </div>
      </PodHeader>

      <PodContent onClick={() => router.push(`/platform-experiments/${pe.id}`)}>
        {pe.description && <CollapsibleDescription text={pe.description} style={{ marginBottom: 12 }} />}

        {pe.metrics && pe.metrics.length > 0 && (
          <div style={{ display: 'flex', gap: 6, flexWrap: 'wrap', marginBottom: 14 }}>
            {pe.metrics.map((m, i) => <MetricChip key={m.key} m={m} primary={i === 0} />)}
          </div>
        )}

        {pe.stages && pe.stages.length > 0 && (
          <div style={{ display: 'flex', gap: 6, flexWrap: 'wrap', alignItems: 'center', marginBottom: 14 }}>
            <span className="uppercase-label" style={{ marginRight: 2 }}>Ladder</span>
            {pe.stages.map((s, i) => {
              const active = i + 1 === pe.current_stage
              return (
                <span key={i} className="mono" title={`Stage ${i + 1}: spans ${s.length_pct}% of the run${s.evict_pct > 0 ? `, then cuts ${s.evict_pct}% of survivors` : ', no cut'}${s.max_job_hours ? `; no job may run longer than ${s.max_job_hours}h` : '; no job length limit'}`} style={{
                  fontSize: 11, padding: '3px 9px', borderRadius: 999,
                  background: active ? 'rgba(96,165,250,0.12)' : 'var(--surface-2)',
                  border: `1px solid ${active ? semantic.accent : 'var(--border)'}`,
                  color: active ? 'var(--foreground)' : 'var(--muted-fg)',
                }}>
                  {s.length_pct}% {s.evict_pct > 0 ? `→ cut ${s.evict_pct}%` : '→ no cut'}{s.max_job_hours ? ` · ≤${s.max_job_hours}h/job` : ''}
                </span>
              )
            })}
          </div>
        )}

        <div style={{ display: 'grid', gridTemplateColumns: 'repeat(4, 1fr)', gap: 16, marginBottom: 14 }}>
          <div>
            <div className="uppercase-label">Budget</div>
            <div className="mono" style={{ fontSize: 20, fontWeight: 700 }}>{pe.budget_accelerator_hours} <span className="text-muted">AccH</span></div>
          </div>
          <div>
            <div className="uppercase-label">Agents</div>
            <div className="mono" style={{ fontSize: 20, fontWeight: 700 }}>{pe.signup_count} <span className="text-muted">/ {pe.max_agents}</span></div>
          </div>
          <div>
            <div className="uppercase-label">Budget Used</div>
            <div className="mono" style={{ fontSize: 20, fontWeight: 700, color: totalUsed > 0 ? 'var(--accent-light)' : undefined }}>
              {formatAccH(totalUsed)} <span className="text-muted">AccH</span>
            </div>
          </div>
          <div>
            <div className="uppercase-label">
              Hypotheses
              {progressPartial && (
                <span className="text-dim" style={{ marginLeft: 6, textTransform: 'none' }}>
                  (first {MAX_LIST_PAGE_SIZE})
                </span>
              )}
            </div>
            <div style={{ display: 'flex', gap: 10, alignItems: 'baseline', flexWrap: 'wrap' }}>
              <span className="mono" style={{ fontSize: 13, color: semantic.success }}>
                {progress.finished} <span className="text-muted" style={{ fontSize: 10 }}>finished</span>
              </span>
              <span className="mono" style={{ fontSize: 13, color: semantic.accent }}>
                {progress.in_flight} <span className="text-muted" style={{ fontSize: 10 }}>in flight</span>
              </span>
              <span className="mono" style={{ fontSize: 13, color: 'var(--muted-fg)' }}>
                {progress.pending} <span className="text-muted" style={{ fontSize: 10 }}>pending</span>
              </span>
            </div>
          </div>
        </div>

        {expanded && (
          <div onClick={e => e.stopPropagation()}>
            <div style={{ borderTop: '1px solid var(--border)', paddingTop: 14 }}>
              <div className="uppercase-label" style={{ marginBottom: 10 }}>
                Agent Quotas — Guaranteed &amp; Burst (AccH)
              </div>
              {!quotas ? (
                <p className="text-dim">Loading quotas…</p>
              ) : quotas.length === 0 ? (
                <p className="text-dim">No quotas allocated yet.</p>
              ) : (
                <table className="wa-table">
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
                          <td className="mono" style={{ fontWeight: 600 }}>{q.agent_id}</td>
                          <td className="mono" style={{ fontSize: 11 }}>{formatAccH(q.used_guaranteed_acch)} / {formatAccH(q.guaranteed_accelerator_hours)} AccH</td>
                          <td className="mono" style={{ fontSize: 11 }}>{formatAccH(q.used_burst_acch)} / {formatAccH(q.burst_accelerator_hours)} AccH</td>
                          <td className="mono" style={{ fontSize: 11, color: remaining > 0 ? semantic.success : semantic.danger }}>{formatAccH(remaining)} AccH</td>
                        </tr>
                      )
                    })}
                  </tbody>
                </table>
              )}
            </div>
          </div>
        )}
      </PodContent>
    </Pod>
  )
}

// ---- Experiment Modal (create / edit) ----

const INPUT_STYLE: React.CSSProperties = {
  width: '100%', padding: '7px 10px', border: '1px solid var(--border)', borderRadius: 6,
  fontSize: 13, fontFamily: 'inherit', boxSizing: 'border-box',
  background: 'var(--surface-2)', color: 'var(--foreground)',
}
const LABEL_STYLE: React.CSSProperties = {
  fontSize: 11, color: 'var(--muted-fg)', textTransform: 'uppercase' as const, letterSpacing: 0.8,
  display: 'block', marginBottom: 4,
}

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

// SubmitterPolicySelect is a 3-way segmented control: only 3 mutually-exclusive options, so a
// dropdown would just cost an extra click. Mirrors the visual language of the stage-ladder
// preset buttons above (active = filled, accent border) rather than introducing a new pattern.
function SubmitterPolicySelect({ value, onChange, disabled }: { value: SubmitterPolicy; onChange: (v: SubmitterPolicy) => void; disabled?: boolean }) {
  return (
    <div style={{ display: 'inline-flex', border: '1px solid var(--border)', borderRadius: 8, overflow: 'hidden' }}>
      {POLICY_OPTIONS.map(opt => {
        const active = value === opt.value
        return (
          <button
            key={opt.value}
            type="button"
            disabled={disabled}
            onClick={() => onChange(opt.value)}
            style={{
              padding: '6px 14px', fontSize: 12, fontWeight: active ? 700 : 400,
              border: 'none', cursor: disabled ? 'not-allowed' : 'pointer',
              background: active ? semantic.accent : 'transparent',
              color: active ? 'var(--background)' : disabled ? 'var(--muted-fg)' : 'var(--foreground)',
              opacity: disabled ? 0.6 : 1,
            }}
          >
            {opt.label}
          </button>
        )
      })}
    </div>
  )
}

// Field wraps one input with its label and the one-line explanation of what it actually does.
function Field({ label, hint, children }: { label: React.ReactNode; hint?: React.ReactNode; children: React.ReactNode }) {
  return (
    <div>
      <label style={LABEL_STYLE}>{label}</label>
      {children}
      {hint && <p className="text-dim" style={{ margin: '4px 0 0', fontSize: 11, lineHeight: 1.5 }}>{hint}</p>}
    </div>
  )
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
      <div style={{ display: 'flex', gap: 6, marginBottom: 8, flexWrap: 'wrap' }}>
        <Button type="button" size="sm" onClick={() => onChange(DEFAULT_STAGES)}>Platform default (40/75, 60/0)</Button>
        <Button type="button" size="sm" onClick={() => onChange([{ length_pct: '100', evict_pct: '0', max_job_hours: '' }])}>Single stage, no cuts</Button>
        <Button type="button" size="sm" onClick={() => onChange([{ length_pct: '20', evict_pct: '50', max_job_hours: '' }, { length_pct: '30', evict_pct: '25', max_job_hours: '' }, { length_pct: '50', evict_pct: '0', max_job_hours: '' }])}>Three stages</Button>
        <Button type="button" size="sm" onClick={() => onChange([{ length_pct: '30', evict_pct: '25', max_job_hours: '1' }, { length_pct: '70', evict_pct: '0', max_job_hours: '' }])}>Explore then commit (1h cap, then none)</Button>
      </div>

      <div style={{ display: 'grid', gridTemplateColumns: 'auto 1fr 1fr 1fr auto', gap: 8, alignItems: 'center' }}>
        <span style={{ ...LABEL_STYLE, marginBottom: 0 }}>#</span>
        <span style={{ ...LABEL_STYLE, marginBottom: 0 }}>Length %</span>
        <span style={{ ...LABEL_STYLE, marginBottom: 0 }}>Evict %</span>
        <span style={{ ...LABEL_STYLE, marginBottom: 0 }}>Max job h</span>
        <span />
        {stages.map((s, i) => (
          <React.Fragment key={i}>
            <span className="mono text-dim" style={{ fontSize: 12 }}>{i + 1}</span>
            <input
              style={INPUT_STYLE} type="number" min={0.1} max={100} step={1} value={s.length_pct}
              onChange={e => setStage(i, { length_pct: e.target.value })}
            />
            <input
              style={{ ...INPUT_STYLE, opacity: i === stages.length - 1 ? 0.5 : 1 }}
              type="number" min={0} max={99} step={5}
              value={s.evict_pct}
              disabled={i === stages.length - 1}
              title={i === stages.length - 1 ? 'The final stage cannot evict — nothing follows it' : undefined}
              onChange={e => setStage(i, { evict_pct: e.target.value })}
            />
            <input
              style={INPUT_STYLE} type="number" min={0} step={0.5} value={s.max_job_hours}
              placeholder="no limit"
              title="Longest a single job may run while this stage is current. Leave empty for no limit."
              onChange={e => setStage(i, { max_job_hours: e.target.value })}
            />
            <button
              type="button"
              onClick={() => onChange(stages.filter((_, idx) => idx !== i).map((st, idx, arr) => idx === arr.length - 1 ? { ...st, evict_pct: '0' } : st))}
              disabled={stages.length <= 1}
              style={{ background: 'none', border: 'none', cursor: stages.length <= 1 ? 'default' : 'pointer', color: 'var(--muted-fg)', opacity: stages.length <= 1 ? 0.3 : 0.7, fontSize: 14 }}
              title="Remove this stage"
            >×</button>
          </React.Fragment>
        ))}
      </div>

      <div style={{ display: 'flex', gap: 8, alignItems: 'center', marginTop: 8 }}>
        <Button
          type="button" size="sm"
          disabled={stages.length >= 8}
          onClick={() => onChange([
            ...stages.slice(0, -1),
            { ...stages[stages.length - 1], evict_pct: '25' },
            { length_pct: '', evict_pct: '0', max_job_hours: '' },
          ])}
        >+ Add stage</Button>
        <span className="mono text-dim" style={{ fontSize: 11, color: Math.abs(total - 100) < 0.001 ? semantic.success : semantic.warning }}>
          total {total}%
        </span>
      </div>

      {err && <p style={{ margin: '6px 0 0', fontSize: 11, color: semantic.warning }}>{err}</p>}
    </div>
  )
}

function MetricsEditor({ metrics, onChange }: { metrics: MetricDefinition[]; onChange: (m: MetricDefinition[]) => void }) {
  const [customKey, setCustomKey] = useState('')
  const [customDesc, setCustomDesc] = useState('')
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
    setCustomDesc('')
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
      <div style={{ display: 'flex', gap: 6, marginBottom: 8 }}>
        <select
          value={selectedPreset}
          onChange={e => setSelectedPreset(e.target.value)}
          style={{ ...INPUT_STYLE, flex: 1 }}
        >
          <option value="">— pick a common metric —</option>
          {COMMON_ML_METRICS.filter(m => m.key !== 'custom').map(m => (
            <option key={m.key} value={m.key}>{m.label} ({m.direction === 'maximize' ? '↑ max' : '↓ min'})</option>
          ))}
        </select>
        <Button type="button" variant="primary" size="sm" onClick={addPreset}>Add</Button>
      </div>

      <div style={{ display: 'flex', gap: 6, marginBottom: 6 }}>
        <input
          style={{ ...INPUT_STYLE, flex: 1 }}
          placeholder="custom_metric_key"
          value={customKey}
          onChange={e => setCustomKey(e.target.value)}
        />
        <select value={customDir} onChange={e => setCustomDir(e.target.value as 'maximize' | 'minimize')} style={{ ...INPUT_STYLE, width: 'auto' }}>
          <option value="maximize">↑ max</option>
          <option value="minimize">↓ min</option>
        </select>
        <Button type="button" size="sm" onClick={addCustom}>Add Custom</Button>
      </div>

      {metrics.length > 0 && (
        <div style={{ display: 'flex', flexWrap: 'wrap', gap: 6, marginTop: 8 }}>
          {metrics.map((m, i) => {
            const primary = i === 0
            return (
              <span key={m.key} style={{
                display: 'inline-flex', alignItems: 'center', gap: 4,
                padding: '3px 8px', borderRadius: 999, fontSize: 12,
                background: m.direction === 'maximize' ? 'rgba(74, 222, 128, 0.12)' : 'rgba(251, 191, 36, 0.12)',
                border: primary ? `1px solid ${semantic.accent}` : `1px solid ${m.direction === 'maximize' ? 'rgba(74, 222, 128, 0.3)' : 'rgba(251, 191, 36, 0.3)'}`,
                boxShadow: primary ? `0 0 0 1px ${semantic.accent} inset` : undefined,
                color: m.direction === 'maximize' ? semantic.success : semantic.warning,
              }}>
                {primary ? (
                  <span title="Primary metric — used for ranking" style={{ fontSize: 9, opacity: 0.85 }}>★</span>
                ) : (
                  <button
                    type="button"
                    onClick={() => makePrimary(m.key)}
                    title="Make primary"
                    style={{ background: 'none', border: 'none', cursor: 'pointer', padding: 0, fontSize: 11, color: 'inherit', opacity: 0.5 }}
                  >
                    ☆
                  </button>
                )}
                <button type="button" onClick={() => toggleDir(m.key)} style={{ background: 'none', border: 'none', cursor: 'pointer', padding: 0, fontSize: 12, color: 'inherit' }}>
                  {m.direction === 'maximize' ? '↑' : '↓'}
                </button>
                {m.key}
                <button type="button" onClick={() => remove(m.key)} style={{ background: 'none', border: 'none', cursor: 'pointer', padding: '0 0 0 2px', fontSize: 11, color: 'inherit', opacity: 0.6 }}>×</button>
              </span>
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
        description: form.description.trim() || '',
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
        await createPlatformExperiment(payload)
      }
      onSaved()
      onClose()
    } catch (err: unknown) {
      setError(err instanceof Error ? err.message : String(err))
    } finally {
      setSubmitting(false)
    }
  }

  const stepTab = (n: 1 | 2, label: string) => (
    <button
      type="button"
      onClick={() => { setError(null); setStep(n) }}
      style={{
        background: 'none', border: 'none', cursor: 'pointer', padding: '0 0 8px', fontSize: 12,
        fontWeight: step === n ? 700 : 400,
        color: step === n ? 'var(--foreground)' : 'var(--muted-fg)',
        borderBottom: step === n ? `2px solid ${semantic.accent}` : '2px solid transparent',
      }}
    >
      {n}. {label}
    </button>
  )

  return (
    <div
      style={{ position: 'fixed', inset: 0, background: 'rgba(2,3,6,0.6)', backdropFilter: 'blur(2px)', zIndex: 100, display: 'flex', alignItems: 'center', justifyContent: 'center' }}
      onClick={onClose}
    >
      <div
        style={{
          background: 'var(--surface-raised)', border: '1px solid var(--border)', borderRadius: 10,
          padding: 28, width: 680, maxWidth: '95vw', maxHeight: '90vh', overflowY: 'auto',
          boxShadow: 'var(--shadow-lg)',
        }}
        onClick={e => e.stopPropagation()}
      >
        <div style={{ fontWeight: 700, fontSize: 16, marginBottom: 14 }}>
          {isEdit ? 'Edit Platform Experiment' : 'New Platform Experiment'}
        </div>

        <div style={{ display: 'flex', gap: 20, borderBottom: '1px solid var(--border)', marginBottom: 18 }}>
          {stepTab(1, 'Essentials')}
          {stepTab(2, 'Competition settings')}
        </div>

        <form onSubmit={handleSubmit}>
          <div style={{ display: 'flex', flexDirection: 'column', gap: 16 }}>
            {step === 1 ? (
              <>
                <Field label="Name *" hint="Shown to every agent in the listing. Make it specific enough to tell two runs apart.">
                  <input style={INPUT_STYLE} value={form.name} onChange={e => set('name', e.target.value)} placeholder="e.g. Q3 Vision Benchmark" />
                </Field>

                <Field
                  label="Description"
                  hint="The brief agents actually read before choosing an approach: the goal, the dataset, the evaluation protocol, and any constraint on how they may reach it."
                >
                  <textarea
                    style={{ ...INPUT_STYLE, minHeight: 80, resize: 'vertical' }}
                    value={form.description}
                    onChange={e => set('description', e.target.value)}
                    placeholder="Goals, evaluation protocol, dataset details, winner criteria…"
                  />
                </Field>

                <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: 14 }}>
                  <Field label="Starts at" hint={lockedWhileRunning ? 'Locked once running.' : 'Signups close and quota is allocated at this moment.'}>
                    <input style={INPUT_STYLE} type="datetime-local" value={form.starts_at} disabled={lockedWhileRunning} onChange={e => set('starts_at', e.target.value)} />
                  </Field>
                  <Field label="Ends at" hint={lockedWhileRunning ? 'Locked once running.' : 'Also drives the ladder clock: progress is whichever runs out first, budget or time.'}>
                    <input style={INPUT_STYLE} type="datetime-local" value={form.ends_at} disabled={lockedWhileRunning} onChange={e => set('ends_at', e.target.value)} />
                  </Field>
                </div>

                <Field
                  label="Optimization metrics"
                  hint={lockedWhileRunning
                    ? 'Locked once running — stage cuts have already ranked agents against these metrics.'
                    : <>The metric keys agents must emit. The <strong>★ primary metric</strong> ranks the leaderboard — click ☆ on another to promote it. The ↑/↓ on each chip sets its direction; click it to flip between maximize and minimize. At a stage cut an agent survives if it survives on <em>at least one</em> metric, so specialists are not eliminated by a metric they never targeted.</>}
                >
                  {lockedWhileRunning ? (
                    <div style={{ display: 'flex', flexWrap: 'wrap', gap: 6 }}>
                      {form.metrics.map(m => (
                        <span key={m.key} className="mono" style={{ fontSize: 11, padding: '3px 9px', borderRadius: 999, background: 'var(--surface-2)', border: '1px solid var(--border)', color: 'var(--muted-fg)' }}>
                          {m.key} {m.direction === 'maximize' ? '↑' : '↓'}
                        </span>
                      ))}
                    </div>
                  ) : (
                    <MetricsEditor metrics={form.metrics} onChange={m => set('metrics', m)} />
                  )}
                </Field>
              </>
            ) : (
              <>
                <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: 14 }}>
                  <Field
                    label="Accelerator budget — AccH (GPU-hours) *"
                    hint={lockedWhileRunning
                      ? 'Locked once running — admission has already committed usage against this budget.'
                      : <>Normalized H100-equivalent hours — the <strong>only</strong> budget (H100 1.0, A100 0.375, L40 0.25/hr). CPU, RAM and storage are capped per job instead, not budgeted.</>}
                  >
                    <input style={INPUT_STYLE} type="number" min={1} step={0.5} value={form.budget_accelerator_hours} disabled={lockedWhileRunning} onChange={e => set('budget_accelerator_hours', e.target.value)} />
                  </Field>
                  <Field label="Max participants" hint={lockedWhileRunning ? 'Locked once running — the roster is fixed for the run.' : 'Signup cap. Signups close when the experiment starts, and the roster is then fixed for the whole run.'}>
                    <input style={INPUT_STYLE} type="number" min={1} max={500} step={1} value={form.max_agents} disabled={lockedWhileRunning} onChange={e => set('max_agents', e.target.value)} />
                  </Field>
                </div>

                <Field
                  label="Max concurrent accelerators"
                  hint={lockedWhileRunning
                    ? 'Locked once running.'
                    : 'Accelerators this experiment may hold in flight (submitted + running) at once, across all agents\' jobs. Empty = platform default. Bounds spend rate now that a job can trigger a cluster scale-up on demand.'}
                >
                  <input style={INPUT_STYLE} type="number" min={1} step={1} placeholder="platform default" value={form.max_concurrent_accelerators} disabled={lockedWhileRunning} onChange={e => set('max_concurrent_accelerators', e.target.value)} />
                </Field>

                <Field
                  label={isEdit ? 'Stage ladder (fixed at creation)' : 'Stage ladder'}
                  hint={isEdit
                    ? <>The ladder is fixed when the experiment is created and cannot be changed afterwards — agents plan around published boundaries, so moving them mid-run would invalidate the competition. Create a new platform experiment to use a different ladder.</>
                    : <>Each stage ends by cutting the worst-performing share of agents (their jobs stop, unspent budget redistributes) — lengths must total 100%, the final stage evicts 0%, and cuts skip entirely at ≤4 agents. <strong>Max job h</strong> caps run length per stage (empty = no limit) and is enforced.</>}
                >
                  {isEdit ? (
                    <div style={{ display: 'flex', gap: 6, flexWrap: 'wrap' }}>
                      {form.stages.map((s, i) => (
                        <span key={i} className="mono" style={{
                          fontSize: 11, padding: '3px 9px', borderRadius: 999,
                          background: 'var(--surface-2)', border: '1px solid var(--border)', color: 'var(--muted-fg)',
                        }}>
                          {i + 1}: {s.length_pct}% {Number(s.evict_pct) > 0 ? `→ cut ${s.evict_pct}%` : '→ no cut'}{s.max_job_hours ? ` · ≤${s.max_job_hours}h/job` : ''}
                        </span>
                      ))}
                    </div>
                  ) : (
                    <StagesEditor stages={form.stages} onChange={s => set('stages', s)} />
                  )}
                </Field>

                <Field
                  label="Report interval (seconds)"
                  hint={lockedWhileRunning
                    ? 'Locked once running.'
                    : "How often agents are expected to push metrics. A job that reports nothing for 3× this interval is treated as dead and evicted, so set it to match your real training loop's logging cadence — too low and healthy jobs get killed during a reschedule."}
                >
                  <input style={INPUT_STYLE} type="number" min={1} step={5} value={form.report_interval_seconds} disabled={lockedWhileRunning} onChange={e => set('report_interval_seconds', e.target.value)} />
                </Field>

                <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: 14 }}>
                  <Field label="Who can submit hypotheses?" hint={POLICY_HINTS.hypothesis[form.hypothesis_submit_policy]}>
                    <SubmitterPolicySelect
                      value={form.hypothesis_submit_policy}
                      disabled={lockedWhileRunning}
                      onChange={v => set('hypothesis_submit_policy', v)}
                    />
                  </Field>
                  <Field label="Who can submit jobs?" hint={POLICY_HINTS.job[form.job_submit_policy]}>
                    <SubmitterPolicySelect
                      value={form.job_submit_policy}
                      disabled={lockedWhileRunning}
                      onChange={v => set('job_submit_policy', v)}
                    />
                  </Field>
                </div>
                {lockedWhileRunning && (
                  <p className="text-dim" style={{ margin: '-8px 0 0', fontSize: 11, lineHeight: 1.5 }}>
                    Locked once running — who may submit has already shaped who signed up.
                  </p>
                )}
              </>
            )}

            {error && <ErrorMessage>{error}</ErrorMessage>}

            <div style={{ display: 'flex', gap: 8, justifyContent: 'flex-end', marginTop: 4 }}>
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
      </div>
    </div>
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

function SortListbox({ value, onChange }: { value: string; onChange: (v: string) => void }) {
  const current = SORT_OPTIONS.find(o => o.value === value) ?? SORT_OPTIONS[0]
  return (
    <Listbox value={value} onChange={onChange} as="div" style={{ position: 'relative' }}>
      <ListboxButton
        className="mono"
        style={{
          fontSize: 12, padding: '5px 10px', border: '1px solid rgba(255,255,255,.16)', borderRadius: 6,
          background: 'var(--surface-2)', color: 'var(--foreground)', cursor: 'pointer', minWidth: 140, textAlign: 'left',
        }}
      >
        Sort: {current.label}
      </ListboxButton>
      <ListboxOptions
        style={{
          position: 'absolute', zIndex: 20, top: '100%', left: 0, marginTop: 4, minWidth: 160,
          border: '1px solid var(--border)', borderRadius: 6, background: 'var(--surface-raised)',
          boxShadow: 'var(--shadow-lg)', padding: 4,
        }}
      >
        {SORT_OPTIONS.map(o => (
          <ListboxOption key={o.value} value={o.value} className="mono">
            {({ focus, selected }: { focus: boolean; selected: boolean }) => (
              <div style={{
                fontSize: 12, padding: '6px 8px', borderRadius: 4, cursor: 'pointer',
                background: focus ? 'var(--surface-2)' : 'transparent',
                color: selected ? 'var(--accent-light)' : 'var(--foreground)',
              }}>
                {o.label}
              </div>
            )}
          </ListboxOption>
        ))}
      </ListboxOptions>
    </Listbox>
  )
}

export default function PlatformExperimentsPage() {
  const [statusFilter, setStatusFilter] = useState('')
  const [search, setSearch] = useState('')
  const [sort, setSort] = useState('-starts_at')
  const [page, setPage] = useState(0)
  const [modal, setModal] = useState<{ open: boolean; editing?: PlatformExperiment | null }>({ open: false })

  const { data, error, isLoading, mutate } = useSWR(
    ['platform-experiments', statusFilter, search, sort, page],
    () => fetchPlatformExperimentsPage({
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
        actions={
          <>
            <Button size="sm" onClick={() => mutate()}>Refresh</Button>
            <Button size="sm" variant="primary" onClick={() => setModal({ open: true, editing: null })}>+ New Experiment</Button>
          </>
        }
      />

      <div style={{ display: 'flex', gap: 12, marginBottom: 16, flexWrap: 'wrap', alignItems: 'center' }}>
        <div style={{ display: 'flex', gap: 6, flexWrap: 'wrap' }}>
          {statuses.map(s => (
            <Chip key={s} active={statusFilter === s} onClick={() => { setStatusFilter(s); setPage(0) }}>
              {s || 'All'}
            </Chip>
          ))}
        </div>

        <input
          value={search}
          onChange={e => { setSearch(e.target.value); setPage(0) }}
          placeholder="Search name or description…"
          className="mono"
          style={{
            fontSize: 12, padding: '5px 10px', border: '1px solid rgba(255,255,255,.16)', borderRadius: 6,
            background: 'var(--surface-2)', color: 'var(--foreground)', minWidth: 220,
          }}
        />

        <SortListbox value={sort} onChange={v => { setSort(v); setPage(0) }} />
      </div>

      {isLoading && <Loading />}
      {error && <ErrorMessage>Failed to load platform experiments — is the stack running?</ErrorMessage>}

      {!isLoading && experiments.length === 0 && (
        <Pod>
          <PodContent style={{ textAlign: 'center', padding: 24 }}>
            <span className="text-dim">No platform experiments found{search ? ` matching "${search}"` : ''}.</span>{' '}
            <Button variant="link" onClick={() => setModal({ open: true })}>
              Create the first one.
            </Button>
          </PodContent>
        </Pod>
      )}

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

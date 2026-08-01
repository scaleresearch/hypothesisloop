'use client'

import React, { useState } from 'react'
import { useRouter } from 'next/navigation'
import useSWR from 'swr'
import {
  fetchPlatformExperiments,
  fetchPlatformExperimentQuotas,
  createPlatformExperiment,
  updatePlatformExperiment,
} from '@/lib/api'
import type { PlatformExperiment, AgentQuota, MetricDefinition } from '@/types'
import type { Stage } from '@/lib/api'
import { COMMON_ML_METRICS } from '@/types'
import { PageHeader } from '@/components/ui/page-header'
import { Pod, PodHeader, PodContent } from '@/components/ui/pod'
import { Badge } from '@/components/ui/badge'
import { Button, Chip } from '@/components/ui/button'
import { MetricBar } from '@/components/ui/metric-bar'
import { Loading, ErrorMessage } from '@/components/ui/status-message'
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

  // Fetched unconditionally (not gated on `expanded`) — the Budget Used / Utilization stat
  // tiles above the expand toggle need this data too, otherwise they show 0 until the card
  // is expanded once.
  const { data: quotas } = useSWR<AgentQuota[]>(
    ['pe-quotas', pe.id],
    () => fetchPlatformExperimentQuotas(pe.id),
    { refreshInterval: 10_000 },
  )

  const totalUsed = (quotas ?? []).reduce((s, q) => s + q.used_guaranteed_acch + q.used_burst_acch, 0)
  const daysLeft = pe.ends_at
    ? Math.ceil((new Date(pe.ends_at).getTime() - Date.now()) / 86_400_000)
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
          {pe.starts_at && (
            <span className="text-dim">{new Date(pe.starts_at).toLocaleDateString()} – {pe.ends_at ? new Date(pe.ends_at).toLocaleDateString() : '?'}</span>
          )}
          {daysLeft != null && daysLeft > 0 && (
            <span style={{ fontSize: 11, color: daysLeft < 2 ? semantic.danger : 'var(--muted-fg)' }}>{daysLeft}d remaining</span>
          )}
          <Button size="sm" onClick={e => { e.stopPropagation(); onEdit(pe) }}>Edit</Button>
          <span className="text-muted" style={{ fontSize: 12 }} onClick={e => { e.stopPropagation(); setExpanded(v => !v) }}>{expanded ? '▲' : '▼'}</span>
        </div>
      </PodHeader>

      <PodContent onClick={() => router.push(`/platform-experiments/${pe.id}`)}>
        {pe.description && (
          <p className="text-dim" style={{ fontSize: 13, marginBottom: 12, whiteSpace: 'pre-wrap' }}>{pe.description}</p>
        )}

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
                <span key={i} className="mono" title={`Stage ${i + 1}: spans ${s.length_pct}% of the run${s.evict_pct > 0 ? `, then cuts ${s.evict_pct}% of survivors` : ', no cut'}`} style={{
                  fontSize: 11, padding: '3px 9px', borderRadius: 999,
                  background: active ? 'rgba(96,165,250,0.12)' : 'var(--surface-2)',
                  border: `1px solid ${active ? semantic.accent : 'var(--border)'}`,
                  color: active ? 'var(--foreground)' : 'var(--muted-fg)',
                }}>
                  {s.length_pct}% {s.evict_pct > 0 ? `→ cut ${s.evict_pct}%` : '→ no cut'}
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
            <div className="uppercase-label">Utilization</div>
            <MetricBar value={totalUsed} max={pe.budget_accelerator_hours} />
          </div>
        </div>

        {(pe.budget_cpu_core_hours || pe.budget_ram_gb_hours || pe.budget_storage_gb_hours) ? (
          <div className="text-dim" style={{ display: 'flex', gap: 16, marginBottom: 14, flexWrap: 'wrap' }}>
            {!!pe.budget_cpu_core_hours && <span>CPU: <span className="mono">{pe.budget_cpu_core_hours}</span> core-h</span>}
            {/* RAM/storage are no longer hours-budgeted (physical fit-only check at admission now) —
                a nonzero value here is a frozen legacy field from before this migration and has no
                effect on scheduling. */}
            {!!pe.budget_ram_gb_hours && (
              <span title="Legacy value — RAM is now a physical fit-only check, not a tracked hours budget">
                RAM: <span className="mono">{pe.budget_ram_gb_hours}</span> GB-h <em style={{ fontStyle: 'normal', opacity: 0.6 }}>(not tracked)</em>
              </span>
            )}
            {!!pe.budget_storage_gb_hours && (
              <span title="Legacy value — storage is now a physical fit-only check, not a tracked hours budget">
                Storage: <span className="mono">{pe.budget_storage_gb_hours}</span> GB-h <em style={{ fontStyle: 'normal', opacity: 0.6 }}>(not tracked)</em>
              </span>
            )}
          </div>
        ) : null}

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
                      const gRem = q.guaranteed_accelerator_hours - q.used_guaranteed_acch
                      const bRem = q.burst_accelerator_hours - q.used_burst_acch
                      return (
                        <tr key={q.id}>
                          <td className="mono" style={{ fontWeight: 600 }}>{q.agent_id}</td>
                          <td className="mono" style={{ fontSize: 11 }}>{formatAccH(q.used_guaranteed_acch)} / {formatAccH(q.guaranteed_accelerator_hours)} AccH</td>
                          <td className="mono" style={{ fontSize: 11 }}>{formatAccH(q.used_burst_acch)} / {formatAccH(q.burst_accelerator_hours)} AccH</td>
                          <td className="mono" style={{ fontSize: 11, color: gRem + bRem > 0 ? semantic.success : semantic.danger }}>{formatAccH(Math.max(0, gRem) + Math.max(0, bRem))} AccH</td>
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
  starts_at: string
  ends_at: string
  metrics: MetricDefinition[]
  report_interval_seconds: number
  stages: Stage[]
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

const DEFAULT_STAGES: Stage[] = [{ length_pct: 40, evict_pct: 75 }, { length_pct: 60, evict_pct: 0 }]

// stagesError mirrors domain.ValidateStages so the form rejects a bad ladder before the API does.
function stagesError(stages: Stage[]): string | null {
  if (stages.length === 0) return 'Add at least one stage.'
  if (stages.length > 8) return 'At most 8 stages.'
  const total = stages.reduce((s, x) => s + Number(x.length_pct), 0)
  if (stages.some(s => Number(s.length_pct) <= 0)) return 'Every stage must be longer than 0%.'
  if (stages.some(s => Number(s.evict_pct) < 0 || Number(s.evict_pct) >= 100)) return 'Evict % must be between 0 and 99.'
  if (total < 99.999 || total > 100.001) return `Stage lengths must add up to 100% (currently ${total}%).`
  if (Number(stages[stages.length - 1].evict_pct) !== 0) return 'The final stage must evict 0% — nothing follows it.'
  return null
}

function StagesEditor({ stages, onChange }: { stages: Stage[]; onChange: (s: Stage[]) => void }) {
  const err = stagesError(stages)
  const total = stages.reduce((s, x) => s + Number(x.length_pct), 0)

  function setStage(i: number, patch: Partial<Stage>) {
    onChange(stages.map((s, idx) => idx === i ? { ...s, ...patch } : s))
  }

  return (
    <div>
      <div style={{ display: 'flex', gap: 6, marginBottom: 8, flexWrap: 'wrap' }}>
        <Button type="button" size="sm" onClick={() => onChange(DEFAULT_STAGES)}>Platform default (40/75, 60/0)</Button>
        <Button type="button" size="sm" onClick={() => onChange([{ length_pct: 100, evict_pct: 0 }])}>Single stage, no cuts</Button>
        <Button type="button" size="sm" onClick={() => onChange([{ length_pct: 20, evict_pct: 50 }, { length_pct: 30, evict_pct: 25 }, { length_pct: 50, evict_pct: 0 }])}>Three stages</Button>
      </div>

      <div style={{ display: 'grid', gridTemplateColumns: 'auto 1fr 1fr auto', gap: 8, alignItems: 'center' }}>
        <span style={{ ...LABEL_STYLE, marginBottom: 0 }}>#</span>
        <span style={{ ...LABEL_STYLE, marginBottom: 0 }}>Length %</span>
        <span style={{ ...LABEL_STYLE, marginBottom: 0 }}>Evict %</span>
        <span />
        {stages.map((s, i) => (
          <React.Fragment key={i}>
            <span className="mono text-dim" style={{ fontSize: 12 }}>{i + 1}</span>
            <input
              style={INPUT_STYLE} type="number" min={0.1} max={100} step={1} value={s.length_pct}
              onChange={e => setStage(i, { length_pct: Number(e.target.value) })}
            />
            <input
              style={{ ...INPUT_STYLE, opacity: i === stages.length - 1 ? 0.5 : 1 }}
              type="number" min={0} max={99} step={5}
              value={s.evict_pct}
              disabled={i === stages.length - 1}
              title={i === stages.length - 1 ? 'The final stage cannot evict — nothing follows it' : undefined}
              onChange={e => setStage(i, { evict_pct: Number(e.target.value) })}
            />
            <button
              type="button"
              onClick={() => onChange(stages.filter((_, idx) => idx !== i).map((st, idx, arr) => idx === arr.length - 1 ? { ...st, evict_pct: 0 } : st))}
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
            { ...stages[stages.length - 1], evict_pct: 25 },
            { length_pct: 0, evict_pct: 0 },
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
  const [form, setForm] = useState<FormState>({
    name: initial?.name ?? '',
    description: initial?.description ?? '',
    budget_accelerator_hours: initial?.budget_accelerator_hours ?? 1000,
    max_agents: initial?.max_agents ?? 20,
    starts_at: initial?.starts_at ? new Date(initial.starts_at).toISOString().slice(0, 16) : localDatetime(1),
    ends_at: initial?.ends_at ? new Date(initial.ends_at).toISOString().slice(0, 16) : localDatetime(8),
    metrics: initial?.metrics ?? [],
    report_interval_seconds: initial?.report_interval_seconds ?? 30,
    stages: initial?.stages?.length ? initial.stages : DEFAULT_STAGES,
  })
  const [step, setStep] = useState<1 | 2>(1)
  const [submitting, setSubmitting] = useState(false)
  const [error, setError] = useState<string | null>(null)

  function set(field: keyof FormState, value: string | number | MetricDefinition[] | Stage[]) {
    setForm(f => ({ ...f, [field]: value }))
  }

  const ladderError = stagesError(form.stages)

  function step1Error(): string | null {
    if (!form.name.trim()) return 'Name is required.'
    if (Number(form.budget_accelerator_hours) <= 0) return 'Total compute budget must be greater than 0.'
    if (new Date(form.ends_at) <= new Date(form.starts_at)) return 'Ends At must be after Starts At.'
    return null
  }

  function goNext() {
    const err = step1Error()
    if (err) { setError(err); return }
    setError(null)
    setStep(2)
  }

  async function handleSubmit(e: React.FormEvent) {
    e.preventDefault()
    // Both steps are validated here, not just the visible one — a bad ladder on step 2 must not
    // be submittable just because step 1 passed.
    const err = step1Error() ?? (isEdit ? null : ladderError)
    if (err) { setError(err); return }
    setSubmitting(true)
    setError(null)
    try {
      const payload = {
        name: form.name.trim(),
        description: form.description.trim() || '',
        budget_accelerator_hours: Number(form.budget_accelerator_hours),
        // Not settable in the form — echoed back unchanged so editing an older experiment that
        // was created with a CPU budget does not silently zero it.
        budget_cpu_core_hours: initial?.budget_cpu_core_hours ?? 0,
        // RAM/storage hours budgets are deprecated/frozen — always submit untracked (0); the backend no
        // longer debits or enforces these fields for new submissions.
        budget_ram_gb_hours: 0,
        budget_storage_gb_hours: 0,
        max_agents: Number(form.max_agents),
        metrics: form.metrics,
        report_interval_seconds: Number(form.report_interval_seconds),
        // Omitted when editing: the backend fixes the ladder at creation and silently ignores
        // it on update, so sending it would imply an edit that never happens.
        ...(isEdit ? {} : { stages: form.stages.map(s => ({ length_pct: Number(s.length_pct), evict_pct: Number(s.evict_pct) })) }),
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
      onClick={() => { if (n === 1) { setError(null); setStep(1) } else { goNext() } }}
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
                  <Field
                    label="Accelerator budget — AccH (GPU-hours) *"
                    hint={<>This is the <strong>only</strong> budget: accelerator-hours, not CPU-hours. AccH is one normalized H100-equivalent hour, so you do not pick a hardware type here — every accelerator bills against this single unit at its own rate (H100 1.0, A100 0.375, L40 0.25). A budget of 100 buys 100 H100-hours <em>or</em> 400 L40-hours. CPU, RAM and storage are not budgeted; they are capped per job at admission so no agent can grab the whole machine.</>}
                  >
                    <input style={INPUT_STYLE} type="number" min={1} step={0.5} value={form.budget_accelerator_hours} onChange={e => set('budget_accelerator_hours', e.target.value)} />
                  </Field>
                  <Field label="Max agents" hint="Signup cap. Signups close when the experiment starts, and the roster is then fixed for the whole run.">
                    <input style={INPUT_STYLE} type="number" min={1} max={500} step={1} value={form.max_agents} onChange={e => set('max_agents', e.target.value)} />
                  </Field>
                </div>

                <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: 14 }}>
                  <Field label="Starts at" hint="Signups close and quota is allocated at this moment.">
                    <input style={INPUT_STYLE} type="datetime-local" value={form.starts_at} onChange={e => set('starts_at', e.target.value)} />
                  </Field>
                  <Field label="Ends at" hint="Also drives the ladder clock: progress is whichever runs out first, budget or time.">
                    <input style={INPUT_STYLE} type="datetime-local" value={form.ends_at} onChange={e => set('ends_at', e.target.value)} />
                  </Field>
                </div>

                <Field
                  label="Optimization metrics"
                  hint={<>The metric keys agents must emit. The <strong>★ primary metric</strong> ranks the leaderboard — click ☆ on another to promote it. The ↑/↓ on each chip sets its direction; click it to flip between maximize and minimize. At a stage cut an agent survives if it survives on <em>at least one</em> metric, so specialists are not eliminated by a metric they never targeted.</>}
                >
                  <MetricsEditor metrics={form.metrics} onChange={m => set('metrics', m)} />
                </Field>
              </>
            ) : (
              <>
                <Field
                  label={isEdit ? 'Stage ladder (fixed at creation)' : 'Stage ladder'}
                  hint={isEdit
                    ? <>The ladder is fixed when the experiment is created and cannot be changed afterwards — agents plan around published boundaries, so moving them mid-run would invalidate the competition. Create a new platform experiment to use a different ladder.</>
                    : <>The run is split into stages. When a stage ends, that share of the <em>surviving</em> agents with the worst metrics is cut: their jobs stop, they cannot resubmit, and their unspent budget is redistributed to the survivors. Lengths must total 100%, and the final stage always evicts 0% because nothing follows it. Cuts are skipped entirely while 4 or fewer agents remain, so a small roster advances without eliminations.</>}
                >
                  {isEdit ? (
                    <div style={{ display: 'flex', gap: 6, flexWrap: 'wrap' }}>
                      {form.stages.map((s, i) => (
                        <span key={i} className="mono" style={{
                          fontSize: 11, padding: '3px 9px', borderRadius: 999,
                          background: 'var(--surface-2)', border: '1px solid var(--border)', color: 'var(--muted-fg)',
                        }}>
                          {i + 1}: {s.length_pct}% {s.evict_pct > 0 ? `→ cut ${s.evict_pct}%` : '→ no cut'}
                        </span>
                      ))}
                    </div>
                  ) : (
                    <StagesEditor stages={form.stages} onChange={s => set('stages', s)} />
                  )}
                </Field>

                <Field
                  label="Report interval (seconds)"
                  hint="How often agents are expected to push metrics. A job that reports nothing for 3× this interval is treated as dead and evicted, so set it to match your real training loop's logging cadence — too low and healthy jobs get killed during a reschedule."
                >
                  <input style={INPUT_STYLE} type="number" min={1} step={5} value={form.report_interval_seconds} onChange={e => set('report_interval_seconds', e.target.value)} />
                </Field>
              </>
            )}

            {error && <ErrorMessage>{error}</ErrorMessage>}

            <div style={{ display: 'flex', gap: 8, justifyContent: 'flex-end', marginTop: 4 }}>
              <Button type="button" onClick={onClose}>Cancel</Button>
              {step === 1 ? (
                <Button type="button" variant="primary" onClick={goNext}>Next: competition settings</Button>
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

export default function PlatformExperimentsPage() {
  const [statusFilter, setStatusFilter] = useState('')
  const [modal, setModal] = useState<{ open: boolean; editing?: PlatformExperiment | null }>({ open: false })

  const { data: experiments, error, isLoading, mutate } = useSWR<PlatformExperiment[]>(
    ['platform-experiments', statusFilter],
    () => fetchPlatformExperiments(statusFilter || undefined),
    { refreshInterval: 10_000 },
  )

  const statuses = ['', 'open', 'running', 'draft', 'closed']
  const counts = (experiments ?? []).reduce((acc, pe) => {
    acc[pe.status] = (acc[pe.status] ?? 0) + 1
    return acc
  }, {} as Record<string, number>)

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

      {experiments && experiments.length > 0 && (
        <div style={{ display: 'flex', gap: 8, marginBottom: 14, flexWrap: 'wrap' }}>
          {(['open', 'running', 'draft', 'closed'] as const).map(s => (
            counts[s] ? (
              <Badge key={s} status={s} style={{ fontSize: 12, padding: '3px 10px' }}>
                {counts[s]} {s}
              </Badge>
            ) : null
          ))}
        </div>
      )}

      <div style={{ display: 'flex', gap: 6, marginBottom: 16, flexWrap: 'wrap' }}>
        {statuses.map(s => (
          <Chip key={s} active={statusFilter === s} onClick={() => setStatusFilter(s)}>
            {s || 'All'}
          </Chip>
        ))}
      </div>

      {isLoading && <Loading />}
      {error && <ErrorMessage>Failed to load platform experiments — is the stack running?</ErrorMessage>}

      {experiments && experiments.length === 0 && (
        <Pod>
          <PodContent style={{ textAlign: 'center', padding: 24 }}>
            <span className="text-dim">No platform experiments found.</span>{' '}
            <Button variant="link" onClick={() => setModal({ open: true })}>
              Create the first one.
            </Button>
          </PodContent>
        </Pod>
      )}

      {experiments?.map(pe => (
        <ExperimentCard
          key={pe.id}
          pe={pe}
          onEdit={pe => setModal({ open: true, editing: pe })}
        />
      ))}
    </div>
  )
}

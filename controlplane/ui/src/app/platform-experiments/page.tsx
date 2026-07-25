'use client'

import { useState } from 'react'
import { useRouter } from 'next/navigation'
import useSWR from 'swr'
import {
  fetchPlatformExperiments,
  fetchPlatformExperimentQuotas,
  createPlatformExperiment,
  updatePlatformExperiment,
} from '@/lib/api'
import type { PlatformExperiment, AgentQuota, MetricDefinition } from '@/types'
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

  const { data: quotas } = useSWR<AgentQuota[]>(
    expanded ? ['pe-quotas', pe.id] : null,
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
  // Optional additional CPU budget. 0 means "not tracked" for this platform experiment
  // (matches the backend's convention — omitting/zeroing leaves that dimension untracked).
  budget_cpu_core_hours: number
  // RAM/storage are deprecated as hours-budget dimensions — they're now a physical fit-only
  // check at admission, not a fair-share budget. No form input for them; always submitted as
  // 0/untracked for new and edited platform experiments.
  max_agents: number
  starts_at: string
  ends_at: string
  metrics: MetricDefinition[]
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
    budget_cpu_core_hours: initial?.budget_cpu_core_hours ?? 0,
    max_agents: initial?.max_agents ?? 20,
    starts_at: initial?.starts_at ? new Date(initial.starts_at).toISOString().slice(0, 16) : localDatetime(1),
    ends_at: initial?.ends_at ? new Date(initial.ends_at).toISOString().slice(0, 16) : localDatetime(8),
    metrics: initial?.metrics ?? [],
  })
  const [submitting, setSubmitting] = useState(false)
  const [error, setError] = useState<string | null>(null)

  function set(field: keyof FormState, value: string | number | MetricDefinition[]) {
    setForm(f => ({ ...f, [field]: value }))
  }

  async function handleSubmit(e: React.FormEvent) {
    e.preventDefault()
    if (!form.name.trim()) { setError('Name is required'); return }
    if (Number(form.budget_accelerator_hours) <= 0) { setError('Budget must be > 0'); return }
    setSubmitting(true)
    setError(null)
    try {
      const payload = {
        name: form.name.trim(),
        description: form.description.trim() || '',
        budget_accelerator_hours: Number(form.budget_accelerator_hours),
        budget_cpu_core_hours: Number(form.budget_cpu_core_hours) || 0,
        // RAM/storage hours budgets are deprecated/frozen — always submit untracked (0); the backend no
        // longer debits or enforces these fields for new submissions.
        budget_ram_gb_hours: 0,
        budget_storage_gb_hours: 0,
        max_agents: Number(form.max_agents),
        metrics: form.metrics,
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

  return (
    <div
      style={{ position: 'fixed', inset: 0, background: 'rgba(2,3,6,0.6)', backdropFilter: 'blur(2px)', zIndex: 100, display: 'flex', alignItems: 'center', justifyContent: 'center' }}
      onClick={onClose}
    >
      <div
        style={{
          background: 'var(--surface-raised)', border: '1px solid var(--border)', borderRadius: 10,
          padding: 28, width: 560, maxWidth: '95vw', maxHeight: '90vh', overflowY: 'auto',
          boxShadow: 'var(--shadow-lg)',
        }}
        onClick={e => e.stopPropagation()}
      >
        <div style={{ fontWeight: 700, fontSize: 16, marginBottom: 20 }}>
          {isEdit ? 'Edit Platform Experiment' : 'New Platform Experiment'}
        </div>

        <form onSubmit={handleSubmit}>
          <div style={{ display: 'flex', flexDirection: 'column', gap: 14 }}>
            <div>
              <label style={LABEL_STYLE}>Name *</label>
              <input style={INPUT_STYLE} value={form.name} onChange={e => set('name', e.target.value)} placeholder="e.g. Q3 Vision Benchmark" />
            </div>

            <div>
              <label style={LABEL_STYLE}>Description</label>
              <textarea
                style={{ ...INPUT_STYLE, minHeight: 80, resize: 'vertical' }}
                value={form.description}
                onChange={e => set('description', e.target.value)}
                placeholder="Goals, evaluation protocol, dataset details, winner criteria…"
              />
            </div>

            <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: 12 }}>
              <div>
                <label style={LABEL_STYLE}>
                  Total Compute Budget * <span style={{ fontWeight: 400, opacity: 0.7, textTransform: 'none' as const }} title="H100-equivalent accelerator-hour — every accelerator type is billed against this one normalized unit (1 AccH = 1 H100-hour), not just literal H100 accelerators">(AccH-equivalent)</span>
                </label>
                <input style={INPUT_STYLE} type="number" min={1} step={0.5} value={form.budget_accelerator_hours} onChange={e => set('budget_accelerator_hours', e.target.value)} />
              </div>
              <div>
                <label style={LABEL_STYLE}>Max Agents</label>
                <input style={INPUT_STYLE} type="number" min={1} max={500} step={1} value={form.max_agents} onChange={e => set('max_agents', e.target.value)} />
              </div>
            </div>

            <div>
              <label style={{ ...LABEL_STYLE, marginBottom: 2 }}>
                Additional resource budget <span style={{ fontWeight: 400, opacity: 0.7 }}>(optional — 0 = not tracked)</span>
              </label>
              <div style={{ display: 'grid', gridTemplateColumns: '1fr', gap: 12 }}>
                <div>
                  <label style={LABEL_STYLE}>CPU-core-hours</label>
                  <input style={INPUT_STYLE} type="number" min={0} step={1} value={form.budget_cpu_core_hours} onChange={e => set('budget_cpu_core_hours', e.target.value)} />
                </div>
              </div>
              <p className="text-dim" style={{ margin: '6px 0 0', fontSize: 11 }}>
                RAM and storage are no longer hours-budgeted — they're checked as a hard physical
                fit at admission (fit-or-reject), not tracked as a fair-share budget, so there's no
                budget to set for them here.
              </p>
            </div>

            <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: 12 }}>
              <div>
                <label style={LABEL_STYLE}>Starts At</label>
                <input style={INPUT_STYLE} type="datetime-local" value={form.starts_at} onChange={e => set('starts_at', e.target.value)} />
              </div>
              <div>
                <label style={LABEL_STYLE}>Ends At</label>
                <input style={INPUT_STYLE} type="datetime-local" value={form.ends_at} onChange={e => set('ends_at', e.target.value)} />
              </div>
            </div>

            <div>
              <label style={LABEL_STYLE}>Optimization Metrics</label>
              <p className="text-dim" style={{ margin: '0 0 8px' }}>
                Define which metrics agents must emit. The <strong>★ primary metric</strong> ranks
                the leaderboard — click ☆ on any other metric to promote it.
              </p>
              <MetricsEditor metrics={form.metrics} onChange={m => set('metrics', m)} />
            </div>

            {error && <ErrorMessage>{error}</ErrorMessage>}

            <div style={{ display: 'flex', gap: 8, justifyContent: 'flex-end', marginTop: 4 }}>
              <Button type="button" onClick={onClose}>Cancel</Button>
              <Button type="submit" variant="primary" disabled={submitting}>
                {submitting ? 'Saving…' : isEdit ? 'Save Changes' : 'Create Experiment'}
              </Button>
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

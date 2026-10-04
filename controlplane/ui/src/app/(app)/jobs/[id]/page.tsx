'use client'

import useSWR from 'swr'
import { useRecordScope } from '@/lib/read-only'
import { TextLink } from '@/components/ui/link'
import {
  LineChart, Line, XAxis, YAxis, CartesianGrid, Tooltip, ResponsiveContainer,
} from 'recharts'
import { fetchExperiment, fetchExperimentMetrics, fetchExperimentLogs } from '@/lib/api'
import type { MetricDataPoint } from '@/types'
import { PlatformExperimentName } from '@/components/platform-experiment-name'
import { Section } from '@/components/ui/section'
import { CodeBlock } from '@/components/ui/code-block'
import { CopyButton } from '@/components/ui/copy-button'
import { Detail, DetailList } from '@/components/ui/detail-list'
import { EmptyState } from '@/components/ui/empty-state'
import { Page } from '@/components/ui/page'
import { Inline, Stack } from '@/components/ui/stack'
import { Code, Pill, Text } from '@/components/ui/text'
import { Badge, TierBadge } from '@/components/ui/badge'
import { Actor } from '@/components/actor'
import { Branch } from '@/components/code-ref'
import { BASELINE_JOB, measuresBaseline } from '@/components/job-hypothesis'
import { Callout, Loading, ErrorMessage } from '@/components/ui/status-message'
import { semantic, seriesPalette, chart } from '@/lib/colors'
import { formatAccH, formatDuration } from '@/lib/format'
import { budgetUnit, kindOf } from '@/lib/units'
import { notAdmittedLabel } from '@/lib/eviction'
import { JobReason } from '@/components/job-reason'
import { graphHref } from '@/lib/hypothesis-graph'

// What eviction_reason says of a job in each status that carries one: a QUEUED job's is the
// reason its last attempt was requeued for.
const LAST_ATTEMPT: Record<string, string> = {
  EVICTED: 'Evicted',
  FAILED: 'Failed',
  REJECTED: 'Rejected',
  QUEUED: 'Requeued, runs again',
}

export default function JobDetailPage({ params }: { params: { id: string } }) {
  const { id } = params

  const { data: job, error } = useSWR(id, fetchExperiment, { refreshInterval: 5_000 })
  useRecordScope(job?.project_id)
  const { data: logs, error: logsError } = useSWR(
    job ? `logs-${id}` : null,
    () => fetchExperimentLogs(id),
    { refreshInterval: 10_000 },
  )
  const { data: metrics, error: metricsError } = useSWR(
    job ? `metrics-${id}` : null,
    () => fetchExperimentMetrics(id),
    { refreshInterval: 10_000 },
  )
  if (error) return (
    <Page title="Job">
      <ErrorMessage>Could not load job {id}: {error.message}</ErrorMessage>
      <TextLink href="/jobs" className="text-sm">← Back to jobs</TextLink>
    </Page>
  )

  if (!job) return <Loading />

  const j = job
  const lastAttempt = LAST_ATTEMPT[j.status]

  // Jobs report one or more metrics (the ranking metric plus any secondary ones the workload
  // emits, e.g. val_loss alongside val_accuracy) — group by metric_name so each gets its
  // own chart instead of interleaving unrelated scales on a single line.
  const primaryMetricName = j.metric?.name
  // The API appends one contiguous block per underlying series (metric_name, or per retry
  // attempt if a retry changed agent_id) rather than a single time-ordered stream, so a job
  // with a retry has two blocks for the same metric_name back to back. Sort each metric's
  // points by when they were recorded before charting, or the connected line jumps backward
  // in time at the block boundary and renders as several crossing lines instead of one.
  const sortedMetrics = [...((metrics ?? []) as MetricDataPoint[])].sort(
    (a, b) => new Date(a.recorded_at as any).getTime() - new Date(b.recorded_at as any).getTime(),
  )
  const seriesByMetric = new Map<string, { frac: number; value: number }[]>()
  // metric_basis defaults to "raw" server-side; a metric whose points carry more than one
  // basis (or any non-"raw" basis) changed what its value means mid-run, and that must be
  // visible next to the chart, not silently plotted as one uniform line.
  const basesByMetric = new Map<string, Set<string>>()
  for (const p of sortedMetrics) {
    const name = p.metric_name
    if (!seriesByMetric.has(name)) seriesByMetric.set(name, [])
    seriesByMetric.get(name)!.push({
      frac: parseFloat((p.fraction_complete * 100).toFixed(1)),
      value: p.metric_value,
    })
    if (!basesByMetric.has(name)) basesByMetric.set(name, new Set())
    basesByMetric.get(name)!.add(p.metric_basis || 'raw')
  }
  const metricNames = Array.from(seriesByMetric.keys()).sort((a, b) => {
    if (a === primaryMetricName) return -1
    if (b === primaryMetricName) return 1
    return a.localeCompare(b)
  })

  const costEstimate = j.estimated_cost_acch != null ? formatAccH(j.estimated_cost_acch) : null
  const baseline = measuresBaseline(j)

  return (
    <Page
        title={
          <>
            <span className="line-clamp-3 min-w-0 text-lg leading-snug" title={j.hypothesis}>{baseline ? BASELINE_JOB : j.hypothesis}</span>
            <Badge status={j.status ?? 'UNKNOWN'} />
          </>
        }
        description={
          <Inline gap="md" className="text-xs">
            <Text as="span" variant="meta" mono tone="default">Job {j.id}</Text>
            <CopyButton value={j.id} />
            {!baseline && <TextLink href={`/hypotheses/${j.hypothesis_id}`} className="whitespace-nowrap">View hypothesis →</TextLink>}
            <TextLink href={graphHref(j.platform_experiment_id, j.hypothesis_id)} className="whitespace-nowrap">In graph →</TextLink>
          </Inline>
        }
        actions={
          <TextLink href={`/jobs?platform_experiment_id=${j.platform_experiment_id}`} className="whitespace-nowrap text-xs">
            ← This experiment&apos;s jobs
          </TextLink>
        }
    >
      {lastAttempt && j.eviction_reason && (
        <Callout tone="warning">
          <span className="font-medium text-warning">{lastAttempt}</span>
          <JobReason reason={j.eviction_reason} />
        </Callout>
      )}

      {/* Why isn't it running / why did it stop. phase_detail is the runtime's own account —
          an image that won't pull, a config the runtime rejected, or the exit code of a
          container that died. For a failed job this is the difference between "it broke" and
          knowing what broke. */}
      {j.phase_detail && (j.phase_detail.reason || j.phase_detail.message) && (
        <Callout tone="danger">
          <span className="font-medium text-danger">Reported by the cluster:</span>{' '}
          {j.phase_detail.reason && <Code className="text-danger">{j.phase_detail.reason}</Code>}
          {j.phase_detail.message && <Text as="span" tone="muted" className="ml-2">{j.phase_detail.message}</Text>}
          {j.phase_detail.restart_count ? (
            <Text as="span" variant="code" tone="muted" className="ml-3">
              restarts: {j.phase_detail.restart_count}
            </Text>
          ) : null}
        </Callout>
      )}

      {/* A queued job never errors on its own; this is the scheduler's current reason. */}
      {j.status === 'QUEUED' && j.not_admitted_reason && (
        <Callout tone="warning">
          <span className="font-medium text-warning">Not admitted:</span>{' '}
          <Text as="span" variant="code" tone="muted">{notAdmittedLabel(j.not_admitted_reason, j.not_admitted_message)}</Text>
        </Callout>
      )}

      {(j.metrics || j.summary || j.status === 'COMPLETED') && (
        <Section title="Result">
          <Stack>
            {(j.metrics ?? []).map(m => (
              <Inline key={m.name} align="baseline" className="font-mono">
                <span className={m.name === primaryMetricName ? 'text-2xl font-medium text-success' : 'text-lg font-medium'}>
                  {m.value.toFixed(4)}
                </span>
                <Text as="span" variant="note">
                  {m.name}{m.basis !== 'raw' && ` · basis ${m.basis}`}
                </Text>
              </Inline>
            ))}
            {j.summary && <Text pre className="max-w-4xl leading-relaxed">{j.summary}</Text>}
            {!j.summary && j.status === 'COMPLETED' && (
              <Text tone="warning">No summary: the agent did not write up what this job found.</Text>
            )}
          </Stack>
        </Section>
      )}

      {/* Job metadata */}
      <Section title="Job details">
        <DetailList>
          <Detail label="Agent"><span title={j.agent_id}>{j.agent_name}</span></Detail>
          <Detail label="Submitted by"><Actor actor={j.actor} /></Detail>
          <Detail label="Experiment">
            <TextLink href={`/platform-experiments/${j.platform_experiment_id}`}><PlatformExperimentName id={j.platform_experiment_id} /></TextLink>
          </Detail>
          <Detail label="Capacity tier"><TierBadge tier={j.capacity_tier} /></Detail>
          <Detail label="Accelerator">{`${j.accelerator_count}× ${j.accelerator_type}`}</Detail>
          <Detail label="Est. duration">{j.estimated_duration_hours != null ? formatDuration(j.estimated_duration_hours * 3_600_000) : '—'}</Detail>
          <Detail label="Est. cost">{costEstimate != null ? `${costEstimate} ${budgetUnit(kindOf(j.accelerator_type))}` : '—'}</Detail>
          <Detail label="Submitted">{new Date(j.created_at).toLocaleString()}</Detail>
          {j.submitted_at && <Detail label="Sent to cluster">{new Date(j.submitted_at).toLocaleString()}</Detail>}
          {j.finished_at && (
            <Detail label="Finished">
              {new Date(j.finished_at).toLocaleString()}
              {j.submitted_at && <span className="text-muted"> · {formatDuration(new Date(j.finished_at).getTime() - new Date(j.submitted_at).getTime())} after it was sent</span>}
            </Detail>
          )}
          {j.theory && <Detail label="Rationale">{j.theory}</Detail>}
          {j.job?.env && Object.keys(j.job.env).length > 0 && (
            <Detail label="Environment">
              <Inline gap="lg" className="gap-y-1">
                {Object.entries(j.job.env).sort().map(([k, v]) => <span key={k}>{k}={v}</span>)}
              </Inline>
            </Detail>
          )}
        </DetailList>
      </Section>

      {/* Metric trajectories — one chart per reported metric, the ranking metric first */}
      <Section title="Metrics over the run">
          {metricsError ? (
            <ErrorMessage>Could not read this job&apos;s metrics: {metricsError.message}</ErrorMessage>
          ) : metricNames.length > 0 ? (
            <div className={metricNames.length > 1 ? 'grid grid-cols-2 gap-4' : 'grid gap-4'}>
              {metricNames.map((name, i) => {
                const bases = Array.from(basesByMetric.get(name) ?? [])
                const nonRaw = bases.filter(b => b !== 'raw')
                const points = seriesByMetric.get(name) ?? []
                // A Line needs 2+ points to draw a segment; a job that reports a metric only
                // once (many one-shot diagnostics do) has exactly 1 point, and dot={false}
                // would render nothing at all for it — real data, empty-looking chart. Show
                // dots whenever there aren't enough points for the line itself to be visible.
                const showDots = points.length < 2
                return (
                <div key={name}>
                  <Text as="div" variant="meta" mono className="mb-1">
                    {name}{name === primaryMetricName ? ' (ranking metric)' : ''}
                    {nonRaw.length > 0 && (
                      <Pill
                        tone="danger"
                        className="ml-2"
                        title="One or more values on this chart are not on the metric's normal ('raw') scale — do not compare them to a raw-basis run."
                      >
                        basis: {bases.join(', ')}
                      </Pill>
                    )}
                  </Text>
                  <ResponsiveContainer width="100%" height={metricNames.length > 1 ? 200 : 260}>
                    <LineChart data={seriesByMetric.get(name)} margin={{ top: 8, right: 16, left: 0, bottom: 14 }}>
                      <CartesianGrid strokeDasharray="3 3" stroke={chart.grid} />
                      <XAxis
                        dataKey="frac"
                        type="number"
                        domain={[0, 100]}
                        tickFormatter={(v) => `${v}%`}
                        tick={chart.tick}
                        stroke={chart.grid}
                        label={{ value: 'Training progress', position: 'insideBottom', offset: -2, ...chart.tick }}
                      />
                      <YAxis domain={['auto', 'auto']} tick={chart.tick} stroke={chart.grid} />
                      <Tooltip
                        formatter={(v: number) => v.toFixed(4)}
                        labelFormatter={(l) => `${l}% complete`}
                        contentStyle={chart.tooltip.contentStyle}
                        labelStyle={chart.tooltip.labelStyle}
                      />
                      <Line
                        dataKey="value"
                        name={name}
                        stroke={name === primaryMetricName ? semantic.primary : seriesPalette[i % seriesPalette.length]}
                        dot={showDots ? { r: 3 } : false}
                        strokeWidth={2}
                        type="monotone"
                      />
                    </LineChart>
                  </ResponsiveContainer>
                </div>
              )})}
            </div>
          ) : (
            <EmptyState>No metric data yet — waiting for job to start reporting.</EmptyState>
          )}
      </Section>

      {/* Lineage */}
      {(j.job?.image || j.code_ref || j.config_hash || j.data_ref) && (
        <Section title="What it ran">
          <DetailList>
            {j.job?.image && <Detail label="Image">{j.job.image}</Detail>}
            {j.branch && <Detail label="Branch"><Branch branch={j.branch} codeRef={j.code_ref} platformExperimentID={j.platform_experiment_id} /></Detail>}
            {j.code_ref && <Detail label="Code (git commit)">{j.code_ref}</Detail>}
            {j.config_hash && <Detail label="Configuration checksum">{j.config_hash}</Detail>}
            {j.data_ref && <Detail label="Data">{j.data_ref}</Detail>}
          </DetailList>
        </Section>
      )}

      {/* Parent lineage */}
      {j.parent_id && (
        <Section title="Derived from">
          <Text variant="note" tone="default">
            Parent job: <TextLink inline href={`/jobs/${j.parent_id}`} className="font-mono">{j.parent_id}</TextLink>
          </Text>
        </Section>
      )}
    
      {/* The job's own output. For a failed job the traceback usually lives only here — the
          record carries a reason code, not the error. Includes the crashed instance's output
          when a container restarted, which is otherwise unreachable. */}
      <Section title="Logs">
          {logsError ? (
            <ErrorMessage>Could not read this job&apos;s logs: {logsError.message}</ErrorMessage>
          ) : logs && logs.length > 0 ? (
            <CodeBlock height="lg">{logs.join('\n')}</CodeBlock>
          ) : (
            <EmptyState>No log output reported yet.</EmptyState>
          )}
      </Section>
    </Page>
  )
}

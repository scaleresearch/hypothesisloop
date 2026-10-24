'use client'

import { useRouter } from 'next/navigation'
import useSWR from 'swr'
import { experimentPath, hypothesisPath, jobPath } from '@/lib/routes'
import { TextLink } from '@/components/ui/link'
import { fetchExperiment, fetchExperimentLogs, fetchExperimentMetrics } from '@/lib/api/jobs'
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
import { Branch, Commit } from '@/components/code-ref'
import { Claim } from '@/components/ui/claim'
import { Prose } from '@/components/ui/prose'
import { BASELINE_JOB, measuresBaseline } from '@/components/job-hypothesis'
import { Callout, Loading, ErrorMessage } from '@/components/ui/status-message'
import { MetricLineChart } from '@/components/metric-line-chart'
import { formatAccH, formatDate, formatDuration, formatMetric, shortId } from '@/lib/format'
import { budgetUnit } from '@/lib/units'
import { notAdmittedLabel } from '@/lib/eviction'
import { JobReason } from '@/components/job-reason'
import { graphHref } from '@/lib/hypothesis-graph'
import { keys } from '@/lib/keys'
import { POLL } from '@/lib/poll'
import { endedStatusValues } from '@/types/api'
import type { MetricDataPoint } from '@/types/api'

// What eviction_reason says of a job in each status that carries one: a QUEUED job's is the
// reason its last attempt was requeued for.
const LAST_ATTEMPT: Record<string, string> = {
  EVICTED: 'Evicted',
  FAILED: 'Failed',
  REJECTED: 'Rejected',
  QUEUED: 'Requeued, runs again',
}

export default function JobDetailPage({ params }: { params: { experimentId: string; jobId: string } }) {
  const { experimentId, jobId: id } = params
  const router = useRouter()

  const { data: job, error } = useSWR(keys.job(id), () => fetchExperiment(id), { refreshInterval: POLL.reports })
  const { data: logs, error: logsError } = useSWR(
    job ? keys.jobLogs(id) : null,
    () => fetchExperimentLogs(id),
    { refreshInterval: POLL.reports },
  )
  const { data: metrics, error: metricsError } = useSWR(
    job ? keys.jobMetrics(id) : null,
    () => fetchExperimentMetrics(id),
    { refreshInterval: POLL.reports },
  )
  // A job is reached through the experiment it ran for, and through no other.
  const elsewhere = job && job.platform_experiment_id !== experimentId
  if (error || elsewhere) return (
    <Page title="Job">
      <ErrorMessage>Could not load job {id}: {elsewhere ? 'it is not a job of this experiment' : error.message}</ErrorMessage>
      <TextLink href={experimentPath(experimentId, 'jobs')} className="text-sm">← This experiment&apos;s jobs</TextLink>
    </Page>
  )

  if (!job) return <Loading />

  const j = job
  const lastAttempt = LAST_ATTEMPT[j.status]

  // Each metric gets its own chart, the ranking metric first, so unrelated scales do not share one.
  const primaryMetricName = j.metric?.name
  const pointsByMetric = new Map<string, MetricDataPoint[]>()
  for (const p of metrics ?? []) pointsByMetric.set(p.metric_name, [...(pointsByMetric.get(p.metric_name) ?? []), p])
  const metricNames = Array.from(pointsByMetric.keys()).sort((a, b) =>
    a === primaryMetricName ? -1 : b === primaryMetricName ? 1 : a.localeCompare(b))

  const costEstimate = j.estimated_cost_acch != null ? formatAccH(j.estimated_cost_acch) : null
  const baseline = measuresBaseline(j)

  return (
    <Page
        title={
          <>
            <span className="min-w-0">{baseline ? BASELINE_JOB : <Claim text={j.hypothesis} lines={2} />}</span>
            <Badge status={j.status ?? 'UNKNOWN'} />
          </>
        }
        description={
          <Inline gap="md" className="text-xs">
            <Text as="span" variant="meta" mono tone="default" title={j.id}>Job {shortId(j.id)}</Text>
            <CopyButton value={j.id} />
            {!baseline && <TextLink href={hypothesisPath(experimentId, j.hypothesis_id!)} className="whitespace-nowrap">View hypothesis →</TextLink>}
            <TextLink href={graphHref(j.platform_experiment_id, j.hypothesis_id)} className="whitespace-nowrap">In graph →</TextLink>
          </Inline>
        }
        back={{ label: 'Jobs', onClick: () => router.push(experimentPath(experimentId, 'jobs')) }}
    >
      {lastAttempt && j.eviction_reason && (
        <Callout tone="warning">
          <span className="font-medium text-warning">{lastAttempt}</span>
          <JobReason reason={j.eviction_reason} outputOpen />
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
                  {formatMetric(m.value)}
                </span>
                <Text as="span" variant="note">
                  {m.name}{m.basis !== 'raw' && ` · basis ${m.basis}`}
                </Text>
              </Inline>
            ))}
            {j.summary && <Prose text={j.summary} linkId={ref => ref.startsWith('job-') ? jobPath(experimentId, ref) : undefined} />}
            {!j.summary && j.status === 'COMPLETED' && (
              <Text tone="warning">No summary: the agent did not write up what this job found.</Text>
            )}
          </Stack>
        </Section>
      )}

      {/* Job metadata */}
      <Section title="Job details">
        <DetailList>
          <Detail label="Agent" mono={false}><span title={j.agent_id}>{j.agent_name}</span></Detail>
          <Detail label="Submitted by" mono={false}><Actor actor={j.actor} /></Detail>
          <Detail label="Experiment" mono={false}>
            <TextLink href={experimentPath(experimentId)}><PlatformExperimentName id={j.platform_experiment_id} /></TextLink>
          </Detail>
          <Detail label="Capacity tier" mono={false}><TierBadge tier={j.capacity_tier} /></Detail>
          <Detail label="Accelerator">{`${j.accelerator_count}× ${j.accelerator_type}`}</Detail>
          <Detail label="Est. duration" mono={false}>{j.estimated_duration_hours != null ? formatDuration(j.estimated_duration_hours * 3_600_000) : '—'}</Detail>
          <Detail label="Est. cost" mono={false}>{costEstimate != null ? `${costEstimate} ${budgetUnit(j.accelerator_kind)}` : '—'}</Detail>
          <Detail label="Submitted" mono={false}>{formatDate(j.created_at)}</Detail>
          {j.submitted_at && <Detail label="Sent to cluster" mono={false}>{formatDate(j.submitted_at)}</Detail>}
          {j.finished_at && (
            <Detail label="Finished" mono={false}>
              {formatDate(j.finished_at)}
              {j.submitted_at && <span className="text-muted"> · {formatDuration(new Date(j.finished_at).getTime() - new Date(j.submitted_at).getTime())} after it was sent</span>}
            </Detail>
          )}
          {j.theory && <Detail label="Rationale" mono={false}><Prose text={j.theory} collapsedLines={4} linkId={ref => ref.startsWith('job-') ? jobPath(experimentId, ref) : undefined} /></Detail>}
          {j.job?.env && Object.keys(j.job.env).length > 0 && (
            <Detail label="Environment" mono={false}>
              <div className="flex flex-wrap gap-1">
                {Object.entries(j.job.env).sort().map(([k, v]) => (
                  <span key={k} className="break-all rounded-sm bg-surface-inset px-1.5 py-0.5 font-mono text-2xs">{k}={v}</span>
                ))}
              </div>
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
              {metricNames.map(name => {
                const points = pointsByMetric.get(name)!
                // A basis other than raw, or a change of basis mid-run, changes what the values mean.
                const bases = Array.from(new Set(points.map(p => p.metric_basis || 'raw')))
                return (
                <div key={name}>
                  <Text as="div" variant="meta" mono className="mb-1">
                    {name}{name === primaryMetricName ? ' (ranking metric)' : ''}
                    {bases.some(b => b !== 'raw') && (
                      <Pill
                        tone="danger"
                        className="ml-2"
                        title="One or more values on this chart are not on the metric's normal ('raw') scale — do not compare them to a raw-basis run."
                      >
                        basis: {bases.join(', ')}
                      </Pill>
                    )}
                  </Text>
                  <div style={{ height: metricNames.length > 1 ? 200 : 260 }}>
                    <MetricLineChart points={points} primary={primaryMetricName} />
                  </div>
                </div>
              )})}
            </div>
          ) : (
            <EmptyState>{(endedStatusValues as readonly string[]).includes(j.status) ? 'This job reported no metric data.' : 'No metric data yet.'}</EmptyState>
          )}
      </Section>

      {/* Lineage */}
      {(j.image || j.code_ref || j.config_hash || j.data_ref) && (
        <Section title="What it ran">
          <DetailList>
            {j.image && <Detail label="Image">{j.image}</Detail>}
            {j.branch && <Detail label="Branch"><Branch branch={j.branch} codeRef={j.code_ref} platformExperimentID={j.platform_experiment_id} /></Detail>}
            {j.code_ref && (
              <Detail label="Code (git commit)">
                <Inline gap="sm" className="gap-y-1">
                  <span className="min-w-0 break-all" title={j.code_ref.slice(0, j.code_ref.lastIndexOf('@'))}>{j.code_ref.slice(0, j.code_ref.lastIndexOf('@'))}</span>
                  <Commit codeRef={j.code_ref} />
                  <CopyButton value={j.code_ref.slice(j.code_ref.lastIndexOf('@') + 1)} />
                </Inline>
              </Detail>
            )}
            {j.config_hash && <Detail label="Configuration checksum">{j.config_hash}</Detail>}
            {j.data_ref && <Detail label="Data">{j.data_ref}</Detail>}
          </DetailList>
        </Section>
      )}

      {/* Parent lineage */}
      {j.parent_id && (
        <Section title="Derived from">
          <Text variant="note" tone="default">
            Parent job: <TextLink inline href={jobPath(experimentId, j.parent_id)} className="font-mono">{j.parent_id}</TextLink>
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

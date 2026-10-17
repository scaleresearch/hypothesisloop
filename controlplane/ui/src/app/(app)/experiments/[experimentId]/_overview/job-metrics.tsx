'use client'

import { useMemo, useState } from 'react'
import useSWR from 'swr'
import { CartesianGrid, Legend, Line, LineChart, ResponsiveContainer, Tooltip, XAxis, YAxis } from 'recharts'
import { fetchExperimentMetrics, fetchExperimentsPage } from '@/lib/api/jobs'
import type { Experiment, MetricDataPoint } from '@/types/jobs'
import { AgentLabel, useAgentName } from '@/components/agent-label'
import { Badge } from '@/components/ui/badge'
import { Captioned } from '@/components/ui/captioned'
import { InsetCard } from '@/components/ui/card'
import { Checkbox } from '@/components/ui/checkbox'
import { EmptyState } from '@/components/ui/empty-state'
import { TextLink } from '@/components/ui/link'
import { SearchableSelect } from '@/components/ui/searchable-select'
import { Inline, Stack } from '@/components/ui/stack'
import { ErrorMessage, Loading } from '@/components/ui/status-message'
import { Text } from '@/components/ui/text'
import { chart, seriesPalette } from '@/lib/colors'
import { shortId } from '@/lib/format'
import { keys } from '@/lib/keys'
import { POLL } from '@/lib/poll'
import { jobPath } from '@/lib/routes'
import { ChartBox, ExpandableChart } from './chart-frame'

// Bounded-score metrics (roughly 0-1) get their own axis so they aren't flattened to a
// hairline by unbounded metrics like seconds_per_subject/cv_seconds sitting in the tens+.
// Kept as a simple name check rather than a generic auto-scaling heuristic.
const BOUNDED_METRIC_RE = /auroc|accuracy|precision|recall|f1|auc/i

function isBoundedMetric(name: string): boolean {
  return BOUNDED_METRIC_RE.test(name)
}

// Shared chart body for the per-job card and the job picker. `trackedMetrics`
// scopes the plot to the experiment's official tracked/ranking metrics rather than every
// ad-hoc metric_name a job happens to report.
function MetricLineChart({ points, trackedMetrics, height }: { points: MetricDataPoint[]; trackedMetrics: string[]; height: number }) {
  const trackedSet = new Set(trackedMetrics)
  const filtered = points.filter((p) => trackedSet.has(p.metric_name))
  const metricNames = Array.from(new Set(filtered.map((p) => p.metric_name))).sort()
  // One row per point; a point only sets its own metric's key, so unrelated metrics are left
  // undefined for that row (a gap in their line, not a fake zero). Plotted over training progress:
  // a job of a few seconds has no readable time axis.
  const chartData = [...filtered]
    .sort((a, b) => new Date(a.recorded_at as string).getTime() - new Date(b.recorded_at as string).getTime())
    .map((p) => ({
      frac: parseFloat((p.fraction_complete * 100).toFixed(1)),
      [p.metric_name]: p.metric_value,
    }))
  if (chartData.length === 0) {
    return <EmptyState>No data yet</EmptyState>
  }
  const hasBounded = metricNames.some(isBoundedMetric)
  const hasUnbounded = metricNames.some((n) => !isBoundedMetric(n))
  const axisOf = (name: string) => (isBoundedMetric(name) ? 'left' : 'right')
  return (
    <ChartBox height={height}>
    <ResponsiveContainer width="100%" height="100%">
      <LineChart data={chartData} margin={{ top: 4, right: 8, left: 0, bottom: 0 }}>
        <CartesianGrid strokeDasharray="3 3" stroke={chart.grid} />
        <XAxis dataKey="frac" type="number" domain={[0, 100]} tickFormatter={(v) => `${v}%`} tick={chart.tick} />
        {hasBounded && <YAxis yAxisId="left" domain={['auto', 'auto']} tick={chart.tick} width={40} />}
        {hasUnbounded && <YAxis yAxisId="right" orientation="right" domain={['auto', 'auto']} tick={chart.tick} width={40} />}
        <Tooltip formatter={(v: number) => v.toFixed(4)} labelFormatter={(l) => `${l}% of training`} contentStyle={chart.tooltip.contentStyle} labelStyle={chart.tooltip.labelStyle} />
        <Legend wrapperStyle={chart.legend} />
        {metricNames.map((name, i) => (
          <Line
            key={name}
            yAxisId={axisOf(name)}
            dataKey={name}
            name={hasBounded && hasUnbounded ? `${name} (${axisOf(name)} axis)` : name}
            stroke={seriesPalette[i % seriesPalette.length]}
            dot={{ r: 2 }}
            strokeWidth={1.5}
            type="monotone"
            connectNulls
            isAnimationActive={false}
          />
        ))}
      </LineChart>
    </ResponsiveContainer>
    </ChartBox>
  )
}

function JobMetricMini({ jobId, trackedMetrics }: { jobId: string; trackedMetrics: string[] }) {
  // 48h-scale experiments move slowly enough that 10s polling here just repeats the same
  // full per-job history over and over; 30s keeps mini-charts fresh without redundant load
  // when dozens of these mount at once (one per running/expanded job).
  const { data: metrics, error } = useSWR<MetricDataPoint[]>(
    keys.jobMetrics(jobId),
    () => fetchExperimentMetrics(jobId),
    { refreshInterval: POLL.slow },
  )
  if (error) return <ErrorMessage>Could not load the metrics of job {jobId}: {error.message}</ErrorMessage>
  if (!metrics) return <Loading />
  return (
    <ExpandableChart label={`Metrics of job ${jobId}`}>
      <MetricLineChart points={metrics.filter(p => p.recorded_at)} trackedMetrics={trackedMetrics} height={400} />
    </ExpandableChart>
  )
}

// Lets the user inspect any job belonging to this platform experiment (not just currently
// running ones) and pick which of that job's actually-reported metrics to chart, rather than
// being limited to the experiment's tracked/ranking subset like the per-agent panel above.
export function LiveMetricsPicker({ jobs }: { jobs: Experiment[] }) {
  const agentName = useAgentName()
  // Most-recently-active first: updated_at moves as a running job reports progress.
  const sorted = useMemo(
    () => [...jobs].sort((a, b) => {
      const tA = new Date(a.updated_at).getTime()
      const tB = new Date(b.updated_at).getTime()
      return tB - tA
    }),
    [jobs],
  )
  const [jobId, setJobId] = useState<string | null>(sorted[0]?.id ?? null)
  const selectedJob = sorted.find(j => j.id === jobId) ?? sorted[0]
  const effectiveJobId = selectedJob?.id

  const { data: metrics, error } = useSWR<MetricDataPoint[]>(
    effectiveJobId ? keys.jobMetrics(effectiveJobId) : null,
    () => fetchExperimentMetrics(effectiveJobId as string),
    { refreshInterval: POLL.slow },
  )
  const points = (metrics ?? []).filter((p) => p.recorded_at)
  const availableMetrics = useMemo(
    () => Array.from(new Set(points.map((p) => p.metric_name))).sort(),
    [points],
  )

  // Selected metric set defaults to all metrics the job actually reports, but is keyed per-job
  // so switching jobs doesn't carry over a selection the new job may not even report.
  const [selectedByJob, setSelectedByJob] = useState<Map<string, Set<string>>>(new Map())
  const selected = selectedByJob.get(effectiveJobId ?? '') ?? new Set(availableMetrics)
  const setSelected = (next: Set<string>) => {
    if (!effectiveJobId) return
    setSelectedByJob(prev => new Map(prev).set(effectiveJobId, next))
  }

  if (!selectedJob) return null

  return (
    <Stack>
      <Inline gap="md" align="start">
        <Captioned caption="Job">
          <SearchableSelect
            options={sorted.map(j => ({ value: j.id, label: `${shortId(j.id)} · ${j.status} · ${agentName(j.agent_id)}` }))}
            value={selectedJob.id}
            onChange={v => { if (v) setJobId(v) }}
            placeholder="Search jobs…"
            className="min-w-80"
            hideAllOption
          />
        </Captioned>
        <Captioned caption="Metrics" className="min-w-52 flex-1">
          {error ? (
            <Text as="span" variant="hint">Unknown</Text>
          ) : !metrics ? (
            <Loading rows={1} />
          ) : availableMetrics.length === 0 ? (
            <Text as="span" variant="hint">No metrics reported yet</Text>
          ) : (
            <Inline gap="xs">
              {availableMetrics.map(name => {
                const isOn = selected.has(name)
                return (
                  <Checkbox
                    key={name}
                    pill
                    checked={isOn}
                    onChange={() => {
                      const next = new Set(selected)
                      isOn ? next.delete(name) : next.add(name)
                      setSelected(next)
                    }}
                    label={name}
                  />
                )
              })}
            </Inline>
          )}
        </Captioned>
      </Inline>
      {error && <ErrorMessage>Could not load the metrics of job {selectedJob.id}: {error.message}</ErrorMessage>}
      {metrics && (
        <ExpandableChart label={`Metrics of job ${selectedJob.id}`}>
          <MetricLineChart points={points} trackedMetrics={Array.from(selected)} height={420} />
        </ExpandableChart>
      )}
    </Stack>
  )
}

export function AgentJobsPanel({ projectId, platformExperimentId, agentId, trackedMetrics }: { projectId: string; platformExperimentId: string; agentId: string; trackedMetrics: string[] }) {
  const { data, error } = useSWR(
    keys.agentJobs(platformExperimentId, agentId),
    () => fetchExperimentsPage({ project_id: projectId, platform_experiment_id: platformExperimentId, agent_id: agentId, sort: '-created_at' }),
  )
  if (error) return <ErrorMessage>Could not load jobs from <AgentLabel id={agentId} />.</ErrorMessage>
  if (!data) return <Loading />
  const jobs = data.items
  if (jobs.length === 0) {
    return <EmptyState>No jobs from <AgentLabel id={agentId} /> yet.</EmptyState>
  }
  return (
    <div className="grid grid-cols-panels gap-3 pb-1 pt-2.5">
      {jobs.map(job => (
        <InsetCard key={job.id} gap="xs">
          <Inline nowrap className="justify-between text-2xs">
            <TextLink href={jobPath(platformExperimentId, job.id)} className="font-mono font-medium">{shortId(job.id)}</TextLink>
            <Badge status={job.status} />
          </Inline>
          <JobMetricMini jobId={job.id} trackedMetrics={trackedMetrics} />
        </InsetCard>
      ))}
    </div>
  )
}

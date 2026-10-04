import type { MetricReference } from '@/lib/api'
import { jobPath } from '@/lib/routes'
import { AgentLabel } from '@/components/agent-label'
import { Badge } from '@/components/ui/badge'
import { TextLink } from '@/components/ui/link'
import { Section } from '@/components/ui/section'
import { Stack } from '@/components/ui/stack'
import { Table, num } from '@/components/ui/table'
import { Text } from '@/components/ui/text'

const counts = {
  true: { tone: 'success', label: 'counts', title: 'The job met every constraint of the goal, so this value counts toward the reference.' },
  false: { tone: 'danger', label: 'broke a constraint', title: 'The job broke a constraint of the goal, so this value does not count.' },
  unknown: { tone: 'neutral', label: 'constraints unreported', title: 'The job did not report every constraint metric, so this value does not count.' },
} as const

/** Every measured value of each outside system the goal compares against, and whether it counts. */
export function ReferenceValues({ experimentId, references }: { experimentId: string; references: MetricReference[] }) {
  const measured = references.filter(r => r.values.length > 0)
  if (measured.length === 0) return null
  return (
    <Section title="References" note="outside systems the goal compares against, never ranked">
      <Stack>
        {measured.map(r => (
          <div key={r.metric}>
            <Text as="div" variant="note" tone="default" className="mb-1 font-medium">
              {r.metric} <span className="font-normal text-subtle">({r.direction})</span>
              {r.best != null && <span className="font-normal">: best that counts {r.best.toFixed(4)}</span>}
              {r.beaten != null && r.leader != null && (
                <span className="font-normal">
                  {', '}{r.beaten ? 'beaten' : 'not beaten'} by the leader&apos;s {r.leader_metric} {r.leader.toFixed(4)}
                </span>
              )}
            </Text>
            <Table>
              <thead>
                <tr>
                  <th>Agent</th>
                  <th className="text-right">Value</th>
                  <th>Constraints</th>
                </tr>
              </thead>
              <tbody>
                {r.values.map((v, i) => {
                  const c = counts[v.constraints_met] ?? counts.unknown
                  return (
                    <tr key={v.experiment_id ?? i}>
                      <td className="font-mono text-xs"><AgentLabel id={v.agent_id} /></td>
                      <td className={num}>
                        {v.experiment_id
                          ? <TextLink href={jobPath(experimentId, v.experiment_id)} title={v.experiment_id}>{v.value.toFixed(4)}</TextLink>
                          : v.value.toFixed(4)}
                      </td>
                      <td><Badge tone={c.tone} title={c.title}>{c.label}</Badge></td>
                    </tr>
                  )
                })}
              </tbody>
            </Table>
          </div>
        ))}
      </Stack>
    </Section>
  )
}

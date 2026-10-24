import { AgentLabel } from '@/components/agent-label'
import { Captioned } from '@/components/ui/captioned'
import { TextLink } from '@/components/ui/link'
import { Section } from '@/components/ui/section'
import { Stack } from '@/components/ui/stack'
import { Claim } from '@/components/ui/claim'
import { Prose } from '@/components/ui/prose'
import { Text } from '@/components/ui/text'
import { formatMetric } from '@/lib/format'
import { hypothesisPath, jobPath } from '@/lib/routes'
import { bestOf } from './standings'
import type { PlatformExperiment, PlatformExperimentResults } from '@/types/api'

/** How a closed experiment ended: its leader on the primary metric and the coordinator's summary. */
export function Outcome({ pe, results, hypothesisText }: {
  pe: PlatformExperiment
  results: PlatformExperimentResults | undefined
  hypothesisText: Map<string, string>
}) {
  const primary = results?.metrics[0]
  const leader = primary?.standings.find(st => st.rank === 1)
  if (!pe.summary && !leader) return null
  const best = leader && bestOf(results, primary!.metric, leader.agent_id)
  return (
    <Section title="Outcome">
      <Stack>
        {leader && (
          <div className="grid gap-1">
            <Text>
              Best {primary!.metric}: <span className="font-medium tabular-nums">{best ? formatMetric(best.best) : '—'}</span>
              {' '}by <AgentLabel id={leader.agent_id} />.
            </Text>
            {leader.hypothesis_id && (
              <Text as="div" className="flex min-w-0 gap-1">
                <span className="flex-none">Testing</span>
                <TextLink href={hypothesisPath(pe.id, leader.hypothesis_id)} className="block min-w-0">
                  <Claim text={hypothesisText.get(leader.hypothesis_id) ?? 'its hypothesis'} lines={1} />
                </TextLink>
              </Text>
            )}
          </div>
        )}
        {pe.summary && (
          <Captioned caption="The coordinator's summary">
            <Prose text={pe.summary} collapsedLines={16} linkId={id => id.startsWith('job-') ? jobPath(pe.id, id) : undefined} />
          </Captioned>
        )}
      </Stack>
    </Section>
  )
}

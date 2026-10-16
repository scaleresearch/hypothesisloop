import type { PlatformExperiment, PlatformExperimentResults } from '@/types/platform-experiments'
import { AgentLabel } from '@/components/agent-label'
import { Captioned } from '@/components/ui/captioned'
import { TextLink } from '@/components/ui/link'
import { Section } from '@/components/ui/section'
import { Stack } from '@/components/ui/stack'
import { Text } from '@/components/ui/text'
import { headline } from '@/lib/format'
import { hypothesisPath } from '@/lib/routes'
import { bestOf } from './standings'

/** How a closed experiment ended: its leader on the primary metric and the coordinator's summary. */
export function Outcome({ pe, results, hypothesisText }: {
  pe: PlatformExperiment
  results: PlatformExperimentResults | undefined
  hypothesisText: Map<string, string>
}) {
  const primary = results?.metrics[0]
  const leader = primary?.standings.find(st => st.rank === 1)
  if (!pe.summary && !leader) return null
  return (
    <Section title="Outcome">
      <Stack>
        {leader && (
          <Text>
            Best {primary!.metric}: <span className="font-medium tabular-nums">{bestOf(results, primary!.metric, leader.agent_id)?.best.toFixed(4) ?? '—'}</span>
            {' '}by <AgentLabel id={leader.agent_id} />
            {leader.hypothesis_id && <>, testing <TextLink inline href={hypothesisPath(pe.id, leader.hypothesis_id)}>{headline(hypothesisText.get(leader.hypothesis_id) ?? 'its hypothesis')}</TextLink></>}.
          </Text>
        )}
        {pe.summary && (
          <Captioned caption="The coordinator's summary">
            <Text pre>{pe.summary}</Text>
          </Captioned>
        )}
      </Stack>
    </Section>
  )
}

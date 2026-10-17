import { Captioned } from '@/components/ui/captioned'
import { CollapsibleDescription } from '@/components/ui/collapsible-description'
import { Section } from '@/components/ui/section'
import { Inline, Stack } from '@/components/ui/stack'
import { Pill } from '@/components/ui/text'
import type { PlatformExperiment } from '@/types/api'

/** The experiment's description and the metrics it optimizes. */
export function About({ pe }: { pe: PlatformExperiment }) {
  const metrics = pe.metrics ?? []
  if (!pe.description && metrics.length === 0) return null
  return (
    <Section title="About this experiment">
      <Stack>
        {pe.description && <CollapsibleDescription text={pe.description} />}
        {metrics.length > 0 && (
          <Captioned caption="Optimization Metrics">
            <Inline>
              {metrics.map((m, i) => (
                <Pill key={m.key} active={i === 0}>
                  {i === 0 && <span className="font-medium">PRIMARY</span>}
                  {m.key} {m.direction === 'maximize' ? '↑ maximize' : '↓ minimize'}
                </Pill>
              ))}
            </Inline>
          </Captioned>
        )}
      </Stack>
    </Section>
  )
}

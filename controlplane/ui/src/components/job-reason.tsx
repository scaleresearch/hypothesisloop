import { evictionCode, evictionCodeLabel, evictionDetailLines } from '@/lib/eviction'
import { CodeBlock } from '@/components/ui/code-block'
import { Disclosure } from '@/components/ui/disclosure'

const OUTPUT_HEADER = 'last output:'

/**
 * Why a job's last attempt ended (eviction_reason): the label of its code and the platform's
 * sentence, with the last lines a failed job said folded under it.
 */
export function JobReason({ reason, outputOpen }: { reason: string; outputOpen?: boolean }) {
  const [sentence, ...rest] = evictionDetailLines(reason)
  const said = rest[0] === OUTPUT_HEADER ? rest.slice(1) : rest
  const label = evictionCodeLabel(evictionCode(reason))
  return (
    <>
      <span className="block">{sentence ? `${label}: ${sentence}` : label}</span>
      {said.length > 0 && (
        // A row link around the reason must not follow a click on the toggle.
        <div onClick={e => e.stopPropagation()} className="mt-1">
          <Disclosure label="Last output" defaultOpen={outputOpen}>
            <CodeBlock framed height="sm">{said.join('\n')}</CodeBlock>
          </Disclosure>
        </div>
      )}
    </>
  )
}

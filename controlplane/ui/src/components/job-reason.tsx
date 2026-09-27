import { evictionCode, evictionCodeLabel, evictionDetailLines } from '@/lib/eviction'

/**
 * Why a job's last attempt ended (eviction_reason): the label of its code and the platform's
 * sentence, with the last lines a failed job said one per row.
 */
export function JobReason({ reason }: { reason: string }) {
  const [sentence, ...said] = evictionDetailLines(reason)
  const label = evictionCodeLabel(evictionCode(reason))
  return (
    <>
      <span className="block">{sentence ? `${label}: ${sentence}` : label}</span>
      {said.map((line, i) => <span key={i} className="block font-mono">{line}</span>)}
    </>
  )
}

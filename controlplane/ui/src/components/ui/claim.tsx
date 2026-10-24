import { clsx } from 'clsx'
import { headline } from '@/lib/format'

/**
 * A hypothesis in a list, a table or a card: its first sentence on one or two lines, the whole
 * claim on hover. The claim's own page shows it in full.
 */
export function Claim({ text, lines = 1, className }: { text: string; lines?: 1 | 2; className?: string }) {
  return <span title={text} className={clsx('break-words', lines === 1 ? 'line-clamp-1' : 'line-clamp-2', className)}>{headline(text)}</span>
}

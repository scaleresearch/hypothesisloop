import { Prose } from '@/components/ui/prose'

/** A long free text, such as an experiment's description: its first lines, the rest one click away. */
export function CollapsibleDescription({ text, lines = 4, className }: { text: string; lines?: number; className?: string }) {
  return <Prose text={text} collapsedLines={lines} className={className} />
}

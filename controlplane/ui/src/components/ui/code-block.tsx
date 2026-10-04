import type { HTMLAttributes } from 'react'
import { clsx } from 'clsx'

interface CodeBlockProps extends HTMLAttributes<HTMLPreElement> {
  /** In a box of its own, for output set inside other text: an exit log. */
  framed?: boolean
  /** Scrolls past a few lines (sm) or a screenful (lg). Without one it shows all of it. */
  height?: 'sm' | 'lg'
}

/** Text as a program wrote it or a person must copy it: a log, a token, an env file. */
export function CodeBlock({ framed, height, className, ...props }: CodeBlockProps) {
  return (
    <pre
      className={clsx(
        'm-0 whitespace-pre-wrap break-words font-mono',
        framed ? 'rounded border bg-surface-inset px-2.5 py-2 text-2xs text-muted' : 'text-xs',
        height && 'overflow-auto',
        height === 'sm' && 'max-h-48',
        height === 'lg' && 'max-h-96',
        className,
      )}
      {...props}
    />
  )
}

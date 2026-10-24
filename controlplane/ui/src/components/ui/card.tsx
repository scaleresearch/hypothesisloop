import type { ComponentProps, HTMLAttributes } from 'react'
import { clsx } from 'clsx'
import { Link } from '@/components/ui/link'
import { Stack } from '@/components/ui/stack'

type DivProps = HTMLAttributes<HTMLDivElement>

/** The panel a page's content sits in. Section is the one with a heading. */
export function Card({ className, interactive, ...props }: DivProps & { interactive?: boolean }) {
  return (
    <div
      className={clsx(
        'animate-rise overflow-hidden rounded-lg border bg-surface',
        interactive && 'cursor-pointer transition hover:border-border-strong hover:shadow-md',
        className,
      )}
      {...props}
    />
  )
}

export function CardContent({ className, scrollX, ...props }: DivProps & { scrollX?: boolean }) {
  return <div className={clsx('p-5', scrollX && 'relative overflow-x-auto', className)} {...props} />
}

const insetEdge = {
  default: 'border-border',
  strong: 'border-border-strong',
  primary: 'border-primary',
} as const

/** A box inside a card, its contents stacked: a per-job chart, a coordinator, a question waiting on an answer. */
export function InsetCard({ className, edge = 'default', gap = 'sm', ...props }: ComponentProps<typeof Stack> & { edge?: keyof typeof insetEdge }) {
  return <Stack gap={gap} className={clsx('rounded border bg-surface-inset px-3.5 py-3', insetEdge[edge], className)} {...props} />
}

/** A row of a list inside a card that leads to the record it names. */
export function RowLink({ className, ...props }: ComponentProps<typeof Link>) {
  return (
    <Link
      className={clsx('flex w-full min-w-0 items-center gap-2 rounded px-2.5 py-1.5 text-xs text-foreground transition-colors hover:bg-hover', className)}
      {...props}
    />
  )
}

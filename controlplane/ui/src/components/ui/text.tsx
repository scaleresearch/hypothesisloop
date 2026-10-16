import type { HTMLAttributes } from 'react'
import { clsx } from 'clsx'
import type { Tone } from '@/lib/colors'
import { toneText, toneTint } from '@/components/ui/tone'

/**
 * The roles a run of text plays, each one step of the type scale in one colour:
 * - body: what is read
 * - note: a sentence about what is beside it
 * - hint: quieter still, what may be skipped
 * - meta: the line under a value: an id, a time, a detail of a table cell
 * - code: an id, a command or a value as typed
 */
const variants = {
  body: 'text-sm',
  note: 'text-xs text-muted',
  hint: 'text-xs text-subtle',
  meta: 'text-2xs text-muted',
  code: 'font-mono text-xs',
} as const

type TextVariant = keyof typeof variants

type TextTone = Tone | 'default' | 'muted' | 'subtle'

const toneClass: Record<TextTone, string> = {
  ...toneText,
  default: 'text-foreground',
  muted: 'text-muted',
  subtle: 'text-subtle',
}

interface TextProps extends HTMLAttributes<HTMLElement> {
  as?: 'p' | 'span' | 'div'
  variant?: TextVariant
  /** Overrides the colour of the variant. */
  tone?: TextTone
  mono?: boolean
  /** Keeps line breaks the text came with. */
  pre?: boolean
}

export function Text({ as: As = 'p', variant = 'body', tone, mono, pre, className, ...props }: TextProps) {
  return <As className={clsx(variants[variant], tone && toneClass[tone], mono && 'font-mono', pre && 'whitespace-pre-wrap break-words', className)} {...props} />
}

/** An id, a command or a value as typed, inside a sentence: mono at the size around it. */
export function Code({ className, ...props }: HTMLAttributes<HTMLElement>) {
  return <code className={clsx('font-mono', className)} {...props} />
}

const headings = {
  1: 'text-2xl font-normal text-foreground',
  2: 'text-base font-medium text-foreground',
  3: 'text-sm font-medium text-foreground',
} as const

/** A page's title (1), a section's (2), or a group's inside a section (3). */
export function Heading({ level, className, ...props }: HTMLAttributes<HTMLHeadingElement> & { level: 1 | 2 | 3 }) {
  const H = `h${level}` as const
  return <H className={clsx(headings[level], className)} {...props} />
}

/** The caption over a value or a group of controls, the same as the label of a FormField. */
export const captionClass = 'text-xs font-medium'

export function Eyebrow({ tone, className, ...props }: HTMLAttributes<HTMLSpanElement> & { tone?: Tone }) {
  return <span className={clsx(captionClass, tone ? toneText[tone] : 'text-muted', className)} {...props} />
}

export function Kbd({ className, ...props }: HTMLAttributes<HTMLElement>) {
  return <kbd className={clsx('flex-none rounded-sm border border-border-strong bg-surface-raised px-1.5 py-px font-mono text-2xs text-muted', className)} {...props} />
}

/** A small rounded label: a metric, a stage, a policy. `tone` tints it, `active` rings the chosen one. */
export function Pill({ tone, active, className, ...props }: HTMLAttributes<HTMLSpanElement> & { tone?: Tone; active?: boolean }) {
  return (
    <span
      className={clsx(
        'inline-flex items-center gap-1 rounded-full border px-2.5 py-0.5 text-xs',
        tone ? [toneText[tone], toneTint[tone]] : 'bg-surface-inset text-muted',
        active && 'border-accent ring-1 ring-inset ring-accent',
        className,
      )}
      {...props}
    />
  )
}

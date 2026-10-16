import type { HTMLAttributes } from 'react'
import { clsx } from 'clsx'

/** The gaps between things, from a label and its value (2xs) to the sections of a page (xl). */
type Gap = '2xs' | 'xs' | 'sm' | 'md' | 'lg' | 'xl'

const gapClass: Record<Gap, string> = {
  '2xs': 'gap-0.5',
  xs: 'gap-1',
  sm: 'gap-2',
  md: 'gap-3',
  lg: 'gap-4',
  xl: 'gap-5',
}

interface LayoutProps extends HTMLAttributes<HTMLElement> {
  as?: 'div' | 'form' | 'label' | 'ul' | 'ol' | 'li' | 'section' | 'aside'
  gap?: Gap
}

interface StackProps extends LayoutProps {
  /** Children keep their own width instead of filling the column. */
  start?: boolean
}

/** Things one under another. A child never grows wider than the column, so long text wraps or truncates. */
export function Stack({ as: As = 'div', gap = 'md', start, className, ...props }: StackProps) {
  return <As className={clsx('grid grid-cols-1 content-start', gapClass[gap], start && 'justify-items-start', className)} {...props} />
}

const alignClass = {
  center: 'items-center',
  // The baseline of their text, for words of different sizes.
  baseline: 'items-baseline',
  // Their bottoms, for fields under labels beside a button.
  end: 'items-end',
  start: 'items-start',
} as const

interface InlineProps extends LayoutProps {
  align?: keyof typeof alignClass
  /** Keeps everything on one line. */
  nowrap?: boolean
}

/** Things side by side, wrapping onto a new line when out of room: a form row, a toolbar, a row of figures. */
export function Inline({ as: As = 'div', gap = 'sm', align = 'center', nowrap, className, ...props }: InlineProps) {
  return <As className={clsx('flex', !nowrap && 'flex-wrap', alignClass[align], gapClass[gap], className)} {...props} />
}

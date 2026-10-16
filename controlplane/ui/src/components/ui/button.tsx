import { forwardRef } from 'react'
import type { ButtonHTMLAttributes, ComponentProps } from 'react'
import NextLink from 'next/link'
import { clsx } from 'clsx'
import { controlHeight } from '@/components/ui/field'
import { Spinner } from '@/components/ui/status-message'

type Variant = 'default' | 'primary' | 'danger' | 'ghost' | 'link'
type Size = 'default' | 'sm' | 'icon'

interface ButtonProps extends ButtonHTMLAttributes<HTMLButtonElement> {
  variant?: Variant
  size?: Size
  /** Shows a spinner and refuses clicks while the action it started is in flight. */
  loading?: boolean
}

const base = 'inline-flex items-center justify-center gap-1.5 whitespace-nowrap font-medium transition disabled:cursor-default'

// The box of field.ts, so a button stands level with the input beside it.
const boxed = 'rounded border usable:active:scale-98'

// Hover is written for anything not disabled, so the same classes serve a link, which is never `:enabled`.
// A primary button that waits for input keeps its shape in the sand of an inset, rather than fading to
// a grey block; one busy with its own action stays ink.
const variantClass: Record<Variant, string> = {
  default: clsx(boxed, 'border-border-control bg-surface text-foreground disabled:opacity-disabled usable:hover:bg-hover'),
  primary: clsx(
    boxed,
    'border-primary bg-primary text-primary-foreground usable:hover:border-primary-hover usable:hover:bg-primary-hover',
    'waiting:border-border-strong waiting:bg-surface-inset waiting:text-subtle',
  ),
  danger: clsx(boxed, 'border-danger-edge bg-danger-tint text-danger disabled:opacity-disabled usable:hover:border-danger'),
  ghost: 'rounded text-muted disabled:opacity-disabled usable:hover:bg-hover usable:hover:text-foreground',
  link: 'text-accent underline-offset-4 disabled:opacity-disabled usable:hover:underline',
}

const sizeClass: Record<Size, string> = {
  default: clsx(controlHeight.default, 'px-4 text-sm'),
  sm: clsx(controlHeight.sm, 'px-3 text-xs'),
  icon: 'size-6 flex-none text-xs',
}

const linkSizeClass: Record<Size, string> = {
  default: 'text-sm',
  sm: 'text-xs',
  icon: 'text-xs',
}

export function Button({ variant = 'default', size = 'default', loading, disabled, className, children, ...props }: ButtonProps) {
  return (
    <button
      className={clsx(base, variantClass[variant], variant === 'link' ? linkSizeClass[size] : sizeClass[size], className)}
      disabled={disabled || loading}
      aria-busy={loading || undefined}
      {...props}
    >
      {loading && <Spinner />}
      {children}
    </button>
  )
}

interface ButtonLinkProps extends ComponentProps<typeof NextLink> {
  variant?: Variant
  size?: Size
}

/** A link that reads as a button, for a step that is a page of its own: Sign in, Console. */
export function ButtonLink({ variant = 'default', size = 'default', className, ...props }: ButtonLinkProps) {
  return <NextLink className={clsx(base, variantClass[variant], variant === 'link' ? linkSizeClass[size] : sizeClass[size], className)} {...props} />
}

interface ChipProps extends ButtonHTMLAttributes<HTMLButtonElement> {
  active?: boolean
}

/** One choice of a filter row; `active` marks the filter in force. */
export function Chip({ active, className, ...props }: ChipProps) {
  return (
    <button
      aria-pressed={active ?? false}
      className={clsx(
        'inline-flex items-center rounded border px-3 text-xs font-medium transition-colors',
        controlHeight.sm,
        active
          ? 'border-primary bg-primary text-primary-foreground'
          : 'border-border-control bg-surface text-muted hover:bg-hover hover:text-foreground',
        className,
      )}
      {...props}
    />
  )
}

interface RowButtonProps extends ButtonHTMLAttributes<HTMLButtonElement> {
  selected?: boolean
  /** Marks a row a search matched. */
  hit?: boolean
}

/** A full-width row of a list that is picked by clicking it: a search hit, an outline entry. */
export const RowButton = forwardRef<HTMLButtonElement, RowButtonProps>(function RowButton({ selected, hit, className, ...props }, ref) {
  return (
    <button
      ref={ref}
      className={clsx(
        'flex w-full min-w-0 items-center gap-2 rounded border px-2.5 py-1.5 text-left text-sm text-foreground transition-colors',
        selected ? 'bg-selected' : 'hover:bg-hover',
        hit ? 'border-warning' : selected ? 'border-accent' : 'border-transparent',
        className,
      )}
      {...props}
    />
  )
})

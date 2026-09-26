import { forwardRef } from 'react'
import type { ButtonHTMLAttributes, ComponentProps } from 'react'
import NextLink from 'next/link'
import { clsx } from 'clsx'
import { Spinner } from '@/components/ui/status-message'

type Variant = 'default' | 'primary' | 'danger' | 'ghost' | 'link'
type Size = 'default' | 'sm' | 'icon'

interface ButtonProps extends ButtonHTMLAttributes<HTMLButtonElement> {
  variant?: Variant
  size?: Size
  /** Shows a spinner and refuses clicks while the action it started is in flight. */
  loading?: boolean
}

const base = 'inline-flex items-center justify-center gap-1.5 font-semibold transition-colors disabled:cursor-default disabled:opacity-50'

const boxed = 'rounded border shadow-sm active:translate-y-px disabled:shadow-none'

// Hover is written for anything not disabled, so the same classes serve a link, which is never `:enabled`.
const variantClass: Record<Variant, string> = {
  default: clsx(boxed, 'border-border-strong bg-surface-raised text-foreground [&:not(:disabled)]:hover:bg-foreground/5'),
  primary: clsx(boxed, 'border-primary bg-primary text-primary-foreground [&:not(:disabled)]:hover:border-primary-hover [&:not(:disabled)]:hover:bg-primary-hover'),
  danger: clsx(boxed, 'border-danger/40 bg-danger/10 text-danger [&:not(:disabled)]:hover:border-danger/60 [&:not(:disabled)]:hover:bg-danger/20'),
  ghost: 'rounded text-muted [&:not(:disabled)]:hover:bg-foreground/5 [&:not(:disabled)]:hover:text-foreground',
  link: 'text-accent [&:not(:disabled)]:hover:underline',
}

const sizeClass: Record<Size, string> = {
  default: 'px-3.5 py-1.5 text-sm',
  sm: 'px-2.5 py-1 text-xs',
  icon: 'h-5 w-5 flex-none text-xs',
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
        'rounded-full border px-3 py-1 text-xs font-semibold transition-colors',
        active
          ? 'border-primary bg-primary text-primary-foreground'
          : 'border-border-strong bg-surface text-muted hover:border-accent hover:bg-foreground/5 hover:text-foreground',
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
        'flex w-full min-w-0 items-center gap-2 rounded border px-2 py-1.5 text-left text-sm text-foreground transition-colors',
        selected ? 'bg-primary/15' : 'hover:bg-foreground/5',
        hit ? 'border-warning/60' : selected ? 'border-primary/40' : 'border-transparent',
        className,
      )}
      {...props}
    />
  )
})

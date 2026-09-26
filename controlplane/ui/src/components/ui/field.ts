import { clsx } from 'clsx'

export type FieldSize = 'default' | 'sm'

/** The box every text control shares: input, textarea, select and the searchable select. */
export function fieldClass(size: FieldSize = 'default', mono?: boolean, bare?: boolean) {
  if (bare) return clsx('bg-transparent text-foreground outline-none placeholder:text-subtle', mono && 'font-mono')
  return clsx(
    'rounded border border-border-control bg-surface-inset text-foreground transition-colors placeholder:text-subtle',
    'hover:border-subtle disabled:cursor-not-allowed disabled:opacity-50 data-[invalid]:border-danger',
    size === 'sm' ? 'px-2 py-1 text-xs' : 'px-2.5 py-1.5 text-sm',
    mono && 'font-mono',
  )
}

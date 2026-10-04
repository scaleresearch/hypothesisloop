import { clsx } from 'clsx'

export type FieldSize = 'default' | 'sm'

/**
 * The box every control shares: input, textarea, select, combobox, button, segmented choice. One
 * radius, one border, one fill, and a height of 36px, or 32px for `sm`; min-h lets a textarea grow
 * from it.
 */
const controlBox = 'rounded border border-border-control bg-surface transition-colors'

const controlSize: Record<FieldSize, string> = {
  default: 'min-h-9 px-3 py-1.75 text-sm',
  sm: 'min-h-8 px-2.5 py-1.5 text-xs',
}

/** The height alone, for a control that draws its own box: a button, a segmented choice. */
export const controlHeight: Record<FieldSize, string> = {
  default: 'h-9',
  sm: 'h-8',
}

export function fieldClass(size: FieldSize = 'default', mono?: boolean, bare?: boolean) {
  if (bare) return clsx('bg-transparent text-foreground outline-none placeholder:text-subtle', mono && 'font-mono')
  return clsx(
    controlBox,
    controlSize[size],
    'text-foreground placeholder:text-subtle',
    'hover:border-muted disabled:cursor-not-allowed disabled:opacity-disabled data-invalid:border-danger',
    mono && 'font-mono',
  )
}

import type { ReactNode } from 'react'
import { Check, ChevronsUpDown } from 'lucide-react'
import { clsx } from 'clsx'
import type { FieldSize } from '@/components/ui/field'

/**
 * The floating list every dropdown opens, at least as wide as its field. Headless UI measures the
 * field into --button-width for a listbox and --input-width for a combobox.
 */
export function panelClass(field: 'button' | 'input') {
  return clsx(
    'z-50 max-h-72 overflow-y-auto rounded border bg-surface-raised p-1.5 shadow-lg focus:outline-none',
    field === 'button' ? 'min-w-[var(--button-width)]' : 'min-w-[var(--input-width)]',
  )
}

/** One row of the list. Headless UI marks the row under the pointer or the arrow keys with data-focus. */
export function optionClass(size: FieldSize = 'default', mono?: boolean) {
  return clsx(
    'group flex cursor-default select-none items-center gap-2 rounded-sm px-2.5 py-1.5 text-foreground',
    'data-[focus]:bg-highlight data-[disabled]:cursor-not-allowed data-[disabled]:opacity-disabled',
    size === 'sm' ? 'text-xs' : 'text-sm',
    mono && 'font-mono',
  )
}

/** A row's text, and the mark of the row that is the current value. */
export function OptionLabel({ children, badge }: { children: ReactNode; badge?: string }) {
  return (
    <>
      <span className="min-w-0 flex-1 truncate group-data-[selected]:font-medium">{children}</span>
      {badge && <span className="flex-none rounded-full border px-1.5 font-sans text-2xs text-muted">{badge}</span>}
      <Check aria-hidden="true" size={14} strokeWidth={2.5} className="invisible flex-none text-accent group-data-[selected]:visible" />
    </>
  )
}

/** A line of the list that is no choice: nothing matched, more exist. */
export function OptionNote({ children }: { children: ReactNode }) {
  return <div className="px-2 py-1.5 text-xs text-subtle">{children}</div>
}

/** The mark at the end of a dropdown's field. */
export function OptionsIcon({ className }: { className?: string }) {
  return <ChevronsUpDown aria-hidden="true" size={13} className={clsx('flex-none text-subtle', className)} />
}

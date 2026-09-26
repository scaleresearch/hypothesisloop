import type { ReactNode } from 'react'
import { Check, ChevronsUpDown } from 'lucide-react'
import { clsx } from 'clsx'
import type { FieldSize } from '@/components/ui/field'

/** The floating list every dropdown opens: Select and SearchableSelect. */
export const panelClass =
  'z-50 max-h-64 min-w-[var(--button-width,var(--input-width))] overflow-y-auto rounded border border-border-strong bg-surface-raised p-1 shadow-lg focus:outline-none'

/** One row of the list. Headless UI marks the row under the pointer or the arrow keys with data-focus. */
export function optionClass(size: FieldSize = 'default', mono?: boolean) {
  return clsx(
    'group flex cursor-default select-none items-center gap-2 rounded-sm px-2 py-1.5 text-foreground',
    'data-[focus]:bg-primary/10 data-[disabled]:cursor-not-allowed data-[disabled]:opacity-50',
    size === 'sm' ? 'text-xs' : 'text-sm',
    mono && 'font-mono',
  )
}

/** A row's text, and the mark of the row that is the current value. */
export function OptionLabel({ children }: { children: ReactNode }) {
  return (
    <>
      <span className="min-w-0 flex-1 truncate group-data-[selected]:font-semibold">{children}</span>
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

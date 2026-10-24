'use client'

import { useEffect, useState } from 'react'
import type { ReactNode } from 'react'
import { Combobox, ComboboxButton, ComboboxInput, ComboboxOptions, ComboboxOption } from '@headlessui/react'
import { clsx } from 'clsx'
import { fieldClass } from '@/components/ui/field'
import type { FieldSize } from '@/components/ui/field'
import { OptionLabel, OptionNote, OptionsIcon, optionClass, panelClass } from '@/components/ui/option'
import { captionClass } from '@/components/ui/text'

interface SearchableSelectOption {
  value: string
  label: string
  /** A word beside the label that sets the option apart: "private". */
  badge?: string
  /** The heading of the rows it shares; groups keep the order of their first option. */
  group?: string
  /** The row, where it says more than the label the field shows once picked. */
  content?: ReactNode
  /** What a search matches besides the label. */
  keywords?: string
}

interface SearchableSelectProps {
  options: SearchableSelectOption[]
  value: string
  onChange: (value: string) => void
  placeholder?: string
  allLabel?: string
  /** Sizes the field; it is at least min-w-44 wide without one. */
  className?: string
  /** Omit the leading "All" row — for pickers where one option must always be selected. */
  hideAllOption?: boolean
  /** Told the query once typing pauses, for a caller that loads its options from a server-side search. Must be stable across renders. */
  onSearch?: (query: string) => void
  /** A dim last row, e.g. how many options the list leaves out. */
  hint?: string
  /** For a list of ids. */
  mono?: boolean
  /** `sm`, the size of a filter row, unless told otherwise. */
  fieldSize?: FieldSize
  /** The most rows shown, the rest left to a search. */
  limit?: number
  'aria-label'?: string
}

function matches(o: SearchableSelectOption, query: string): boolean {
  const text = `${o.label} ${o.keywords ?? ''}`.toLowerCase()
  return query.toLowerCase().split(/\s+/).every(term => text.includes(term))
}

function grouped(options: SearchableSelectOption[]): [string | undefined, SearchableSelectOption[]][] {
  const groups = new Map<string | undefined, SearchableSelectOption[]>()
  for (const o of options) groups.set(o.group, [...(groups.get(o.group) ?? []), o])
  return Array.from(groups)
}

/**
 * The Select for a list long enough that typing beats scrolling. Options are shown in the order
 * passed in, so the caller decides the sort.
 */
export function SearchableSelect({ options, value, onChange, placeholder, allLabel = 'All', className, hideAllOption, onSearch, hint, mono, fieldSize = 'sm', limit, 'aria-label': ariaLabel }: SearchableSelectProps) {
  const [query, setQuery] = useState('')
  useEffect(() => {
    if (!onSearch) return
    const timer = setTimeout(() => onSearch(query), 200)
    return () => clearTimeout(timer)
  }, [query, onSearch])

  // A value outside the options (a filter set from a link) shows as itself, never as "All".
  const selected = options.find(o => o.value === value)
  const filtered = query.trim() === '' || onSearch
    ? options
    : options.filter(o => matches(o, query.trim()))
  const shown = limit === undefined ? filtered : filtered.slice(0, limit)
  const more = filtered.length - shown.length

  return (
    <Combobox
      immediate
      value={value}
      onChange={(v: string | null) => onChange(v ?? '')}
      onClose={() => setQuery('')}
      as="div"
      className={clsx('relative', className ?? 'min-w-44')}
    >
      {({ open }) => (
        <>
          <ComboboxInput
            aria-label={ariaLabel}
            // The list opens when the field takes focus, and the field keeps focus after a choice,
            // so a click on it while the list is closed opens the list the way ArrowDown does.
            onClick={e => {
              if (!open) e.currentTarget.dispatchEvent(new KeyboardEvent('keydown', { key: 'ArrowDown', bubbles: true }))
            }}
            className={clsx(fieldClass(fieldSize, mono), 'w-full truncate pr-7 data-open:border-accent')}
            placeholder={placeholder ?? allLabel}
            displayValue={() => selected?.label ?? value}
            onChange={e => setQuery(e.target.value)}
          />
          <ComboboxButton aria-label="Show options" className="absolute inset-y-0 right-0 flex items-center px-2">
            <OptionsIcon />
          </ComboboxButton>
          <ComboboxOptions anchor={{ to: 'bottom start', gap: 4, padding: 8 }} className={panelClass('input')}>
            {!hideAllOption && (
              <ComboboxOption value="" className={optionClass(fieldSize, mono)}>
                <OptionLabel>{allLabel}</OptionLabel>
              </ComboboxOption>
            )}
            {shown.length === 0 ? <OptionNote>No matches</OptionNote> : grouped(shown).map(([group, rows]) => (
              <div key={group ?? ''} role={group ? 'group' : undefined} aria-label={group}>
                {group && <div aria-hidden="true" className={clsx(captionClass, 'px-2.5 pb-1 pt-2 text-muted')}>{group}</div>}
                {rows.map(o => (
                  <ComboboxOption key={o.value} value={o.value} className={optionClass(fieldSize, mono)}>
                    <OptionLabel badge={o.badge}>{o.content ?? o.label}</OptionLabel>
                  </ComboboxOption>
                ))}
              </div>
            ))}
            {more > 0 && <OptionNote>{more} more, type to search</OptionNote>}
            {hint && <OptionNote>{hint}</OptionNote>}
          </ComboboxOptions>
        </>
      )}
    </Combobox>
  )
}

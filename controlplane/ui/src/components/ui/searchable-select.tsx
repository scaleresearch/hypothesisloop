'use client'

import { useEffect, useState } from 'react'
import { Combobox, ComboboxButton, ComboboxInput, ComboboxOptions, ComboboxOption } from '@headlessui/react'
import { clsx } from 'clsx'
import { fieldClass } from '@/components/ui/field'
import { OptionLabel, OptionNote, OptionsIcon, optionClass, panelClass } from '@/components/ui/option'

interface SearchableSelectOption {
  value: string
  label: string
}

interface SearchableSelectProps {
  options: SearchableSelectOption[]
  value: string
  onChange: (value: string) => void
  placeholder?: string
  allLabel?: string
  /** Sizes the field; it is at least 180px wide without one. */
  className?: string
  /** Omit the leading "All" row — for pickers where one option must always be selected. */
  hideAllOption?: boolean
  /** Told the query once typing pauses, for a caller that loads its options from a server-side search. Must be stable across renders. */
  onSearch?: (query: string) => void
  /** A dim last row, e.g. how many options the list leaves out. */
  hint?: string
  /** For a list of ids. */
  mono?: boolean
}

/**
 * The Select for a list long enough that typing beats scrolling. Options are shown in the order
 * passed in, so the caller decides the sort.
 */
export function SearchableSelect({ options, value, onChange, placeholder, allLabel = 'All', className, hideAllOption, onSearch, hint, mono }: SearchableSelectProps) {
  const [query, setQuery] = useState('')
  useEffect(() => {
    if (!onSearch) return
    const timer = setTimeout(() => onSearch(query), 200)
    return () => clearTimeout(timer)
  }, [query, onSearch])

  // A value outside the options (a filter set from a link) shows as itself, never as "All".
  const selected = options.find(o => o.value === value)
  const filtered = query === '' || onSearch
    ? options
    : options.filter(o => o.label.toLowerCase().includes(query.toLowerCase()))

  return (
    <Combobox
      immediate
      value={value}
      onChange={(v: string | null) => onChange(v ?? '')}
      onClose={() => setQuery('')}
      as="div"
      className={clsx('relative', className ?? 'min-w-[180px]')}
    >
      {({ open }) => (
        <>
          <ComboboxInput
            // The list opens when the field takes focus, and the field keeps focus after a choice,
            // so a click on it while the list is closed opens the list the way ArrowDown does.
            onClick={e => {
              if (!open) e.currentTarget.dispatchEvent(new KeyboardEvent('keydown', { key: 'ArrowDown', bubbles: true }))
            }}
            className={clsx(fieldClass('sm', mono), 'w-full truncate pr-7 data-[open]:border-accent')}
            placeholder={placeholder ?? allLabel}
            displayValue={() => selected?.label ?? value}
            onChange={e => setQuery(e.target.value)}
          />
          <ComboboxButton aria-label="Show options" className="absolute inset-y-0 right-0 flex items-center px-2">
            <OptionsIcon />
          </ComboboxButton>
          <ComboboxOptions anchor={{ to: 'bottom start', gap: 4, padding: 8 }} className={panelClass}>
            {!hideAllOption && (
              <ComboboxOption value="" className={optionClass('sm', mono)}>
                <OptionLabel>{allLabel}</OptionLabel>
              </ComboboxOption>
            )}
            {filtered.length === 0 ? <OptionNote>No matches</OptionNote> : filtered.map(o => (
              <ComboboxOption key={o.value} value={o.value} className={optionClass('sm', mono)}>
                <OptionLabel>{o.label}</OptionLabel>
              </ComboboxOption>
            ))}
            {hint && <OptionNote>{hint}</OptionNote>}
          </ComboboxOptions>
        </>
      )}
    </Combobox>
  )
}

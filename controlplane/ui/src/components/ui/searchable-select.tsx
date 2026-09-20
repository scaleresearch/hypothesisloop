'use client'

import { useEffect, useState } from 'react'
import { Combobox, ComboboxInput, ComboboxOptions, ComboboxOption } from '@headlessui/react'
import { clsx } from 'clsx'
import { fieldClass } from '@/components/ui/field'

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
}

function optionClass(focus: boolean, selected: boolean) {
  return clsx('cursor-pointer rounded-sm px-2 py-1.5 font-mono text-xs', focus && 'bg-foreground/10', selected ? 'text-accent' : 'text-foreground')
}

/**
 * Searchable, keyboard-navigable dropdown (Headless UI Combobox) — a drop-in replacement for a
 * raw <select> when the option list can grow long enough that scanning it beats scrolling it.
 * Options are shown in the order passed in, so callers control sort (e.g. newest first).
 */
export function SearchableSelect({ options, value, onChange, placeholder, allLabel = 'All', className, hideAllOption, onSearch, hint }: SearchableSelectProps) {
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
        className={clsx(fieldClass('sm', true), 'w-full')}
        placeholder={placeholder ?? allLabel}
        displayValue={() => selected?.label ?? value}
        onChange={e => setQuery(e.target.value)}
      />
      <ComboboxOptions className="absolute inset-x-0 top-full z-20 mt-1 max-h-64 overflow-y-auto rounded border bg-surface-raised p-1 shadow-lg">
        {!hideAllOption && (
          <ComboboxOption value="">
            {({ focus }: { focus: boolean }) => <div className={optionClass(focus, value === '')}>{allLabel}</div>}
          </ComboboxOption>
        )}
        {filtered.length === 0 ? (
          <div className="px-2 py-1.5 text-xs text-subtle">No matches</div>
        ) : filtered.map(o => (
          <ComboboxOption key={o.value} value={o.value}>
            {({ focus }: { focus: boolean }) => <div className={optionClass(focus, o.value === value)}>{o.label}</div>}
          </ComboboxOption>
        ))}
        {hint && <div className="px-2 py-1.5 text-xs text-subtle">{hint}</div>}
      </ComboboxOptions>
        </>
      )}
    </Combobox>
  )
}

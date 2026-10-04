'use client'

import { useState } from 'react'
import { Combobox as HeadlessCombobox, ComboboxInput, ComboboxOption, ComboboxOptions } from '@headlessui/react'
import { clsx } from 'clsx'
import { fieldClass } from '@/components/ui/field'
import { OptionLabel, optionClass, panelClass } from '@/components/ui/option'

export interface ComboboxChoice<T> {
  key: string
  value: T
  label: string
  /** A word beside the label: a direction, a kind. */
  badge?: string
}

interface ComboboxProps<T> {
  'aria-label': string
  placeholder?: string
  /** The choices for what is typed so far; one may be what was typed itself. */
  choices: (query: string) => ComboboxChoice<T>[]
  onPick: (value: T) => void
}

/** A field that adds one thing at a time: type to narrow the choices, or to name a new one, and pick. */
export function Combobox<T>({ 'aria-label': ariaLabel, placeholder, choices, onPick }: ComboboxProps<T>) {
  const [query, setQuery] = useState('')
  return (
    <HeadlessCombobox<T | null>
      immediate
      value={null}
      onChange={v => { if (v !== null) { onPick(v); setQuery('') } }}
      onClose={() => setQuery('')}
    >
      <ComboboxInput
        aria-label={ariaLabel}
        className={clsx(fieldClass(), 'w-full')}
        placeholder={placeholder}
        displayValue={() => query}
        onChange={e => setQuery(e.target.value)}
      />
      <ComboboxOptions anchor={{ to: 'bottom start', gap: 4, padding: 8 }} className={panelClass('input')}>
        {choices(query.trim()).map(choice => (
          <ComboboxOption key={choice.key} value={choice.value} className={optionClass()}>
            <OptionLabel badge={choice.badge}>{choice.label}</OptionLabel>
          </ComboboxOption>
        ))}
      </ComboboxOptions>
    </HeadlessCombobox>
  )
}

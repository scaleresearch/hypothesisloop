'use client'

import { Field, Label, Listbox, ListboxButton, ListboxOption, ListboxOptions } from '@headlessui/react'
import { clsx } from 'clsx'
import { fieldClass } from '@/components/ui/field'
import type { FieldSize } from '@/components/ui/field'
import { OptionLabel, OptionsIcon, optionClass, panelClass } from '@/components/ui/option'

export interface SelectOption<T extends string = string> {
  value: T
  label: string
  disabled?: boolean
}

interface SelectProps<T extends string> {
  value: T
  onChange: (value: T) => void
  options: SelectOption<T>[]
  /** Shown while the value is none of the options. */
  placeholder?: string
  /** A dim word before the value, for a control that names itself: "Sort". */
  prefix?: string
  fieldSize?: FieldSize
  mono?: boolean
  invalid?: boolean
  disabled?: boolean
  /** Sizes the field. */
  className?: string
  /** Names the control where no FormField around it does. */
  'aria-label'?: string
}

/** A dropdown for a short fixed list; a list that grows takes SearchableSelect. */
export function Select<T extends string>({
  value, onChange, options, placeholder, prefix, fieldSize, mono, invalid, disabled, className, 'aria-label': ariaLabel,
}: SelectProps<T>) {
  const selected = options.find(o => o.value === value)
  const listbox = (
    <Listbox value={value} onChange={onChange} disabled={disabled} invalid={invalid}>
      <ListboxButton
        className={clsx(
          fieldClass(fieldSize, mono),
          'inline-flex cursor-pointer items-center justify-between gap-2 text-left data-[open]:border-accent',
          className,
        )}
      >
        <span className="min-w-0 truncate">
          {prefix && <span className="text-subtle">{prefix} </span>}
          {selected ? selected.label : <span className="text-subtle">{placeholder}</span>}
        </span>
        <OptionsIcon />
      </ListboxButton>
      <ListboxOptions modal={false} anchor={{ to: 'bottom start', gap: 4, padding: 8 }} className={panelClass}>
        {options.map(option => (
          <ListboxOption key={option.value} value={option.value} disabled={option.disabled} className={optionClass(fieldSize, mono)}>
            <OptionLabel>{option.label}</OptionLabel>
          </ListboxOption>
        ))}
      </ListboxOptions>
    </Listbox>
  )
  if (!ariaLabel) return listbox
  return (
    <Field className="contents">
      <Label className="sr-only">{ariaLabel}</Label>
      {listbox}
    </Field>
  )
}

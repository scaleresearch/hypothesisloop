'use client'

import { useState } from 'react'
import { Combobox, ComboboxInput, ComboboxOptions, ComboboxOption } from '@headlessui/react'

export interface SearchableSelectOption {
  value: string
  label: string
}

interface SearchableSelectProps {
  options: SearchableSelectOption[]
  value: string
  onChange: (value: string) => void
  placeholder?: string
  allLabel?: string
  className?: string
}

const FIELD_STYLE: React.CSSProperties = {
  fontSize: 12,
  padding: '5px 10px',
  border: '1px solid rgba(255,255,255,.16)',
  borderRadius: 6,
  background: 'var(--surface-2)',
  color: 'var(--foreground)',
  width: '100%',
  boxSizing: 'border-box',
}

function optionRowStyle(focus: boolean, selected: boolean): React.CSSProperties {
  return {
    fontSize: 12, padding: '6px 8px', borderRadius: 4, cursor: 'pointer',
    background: focus ? 'var(--surface-2)' : 'transparent',
    color: selected ? 'var(--accent-light)' : 'var(--foreground)',
  }
}

/**
 * Searchable, keyboard-navigable dropdown (Headless UI Combobox) — a drop-in replacement for a
 * raw <select> when the option list can grow long enough that scanning it beats scrolling it.
 * Options are shown in the order passed in, so callers control sort (e.g. newest first).
 */
export function SearchableSelect({ options, value, onChange, placeholder, allLabel = 'All', className }: SearchableSelectProps) {
  const [query, setQuery] = useState('')

  const selected = options.find(o => o.value === value) ?? null
  const filtered = query === ''
    ? options
    : options.filter(o => o.label.toLowerCase().includes(query.toLowerCase()))

  return (
    <Combobox
      value={value}
      onChange={(v: string | null) => { onChange(v ?? ''); setQuery('') }}
      as="div"
      className={className}
      style={{ position: 'relative', minWidth: 180 }}
    >
      <ComboboxInput
        className="mono"
        style={FIELD_STYLE}
        placeholder={placeholder ?? allLabel}
        displayValue={() => (selected ? selected.label : '')}
        onChange={e => setQuery(e.target.value)}
      />
      <ComboboxOptions
        style={{
          position: 'absolute', zIndex: 20, top: '100%', left: 0, right: 0, marginTop: 4,
          maxHeight: 260, overflowY: 'auto', border: '1px solid var(--border)', borderRadius: 6,
          background: 'var(--surface-raised)', boxShadow: 'var(--shadow-lg)', padding: 4,
        }}
      >
        <ComboboxOption value="" className="mono">
          {({ focus }: { focus: boolean }) => <div style={optionRowStyle(focus, value === '')}>{allLabel}</div>}
        </ComboboxOption>
        {filtered.length === 0 ? (
          <div className="text-dim" style={{ fontSize: 12, padding: '6px 8px' }}>No matches</div>
        ) : filtered.map(o => (
          <ComboboxOption key={o.value} value={o.value} className="mono">
            {({ focus }: { focus: boolean }) => <div style={optionRowStyle(focus, o.value === value)}>{o.label}</div>}
          </ComboboxOption>
        ))}
      </ComboboxOptions>
    </Combobox>
  )
}

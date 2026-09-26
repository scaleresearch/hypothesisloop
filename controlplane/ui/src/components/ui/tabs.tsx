import { clsx } from 'clsx'

interface TabsProps<T extends string> {
  label: string
  tabs: { id: T; label: string }[]
  value: T
  onChange: (id: T) => void
}

/** A row of underlined tabs; the caller renders the panel of the chosen one. */
export function Tabs<T extends string>({ label, tabs, value, onChange }: TabsProps<T>) {
  return (
    <div role="tablist" aria-label={label} className="flex gap-4 border-b">
      {tabs.map(tab => (
        <button
          key={tab.id}
          type="button"
          role="tab"
          aria-selected={tab.id === value}
          onClick={() => onChange(tab.id)}
          className={clsx(
            '-mb-px border-b-2 pb-2 text-xs transition-colors',
            tab.id === value ? 'border-accent font-bold text-foreground' : 'border-transparent text-muted hover:text-foreground',
          )}
        >
          {tab.label}
        </button>
      ))}
    </div>
  )
}

interface SegmentedProps<T extends string> {
  label: string
  options: { value: T; label: string }[]
  value: T
  onChange: (value: T) => void
  disabled?: boolean
}

/** A few exclusive choices side by side, one always chosen. */
export function Segmented<T extends string>({ label, options, value, onChange, disabled }: SegmentedProps<T>) {
  return (
    <div role="group" aria-label={label} className="inline-flex overflow-hidden rounded border border-border-control">
      {options.map(option => (
        <button
          key={option.value}
          type="button"
          disabled={disabled}
          aria-pressed={option.value === value}
          onClick={() => onChange(option.value)}
          className={clsx(
            'px-3.5 py-1.5 text-xs transition-colors disabled:cursor-not-allowed disabled:opacity-50',
            option.value === value ? 'bg-primary font-bold text-primary-foreground' : 'bg-surface-inset text-muted enabled:hover:text-foreground',
          )}
        >
          {option.label}
        </button>
      ))}
    </div>
  )
}

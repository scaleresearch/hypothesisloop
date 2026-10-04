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
    <div role="tablist" aria-label={label} className="flex gap-6 border-b">
      {tabs.map(tab => (
        <button
          key={tab.id}
          type="button"
          role="tab"
          aria-selected={tab.id === value}
          onClick={() => onChange(tab.id)}
          className={clsx(
            '-mb-px border-b-2 pb-2.5 text-sm transition-colors',
            tab.id === value ? 'border-accent text-foreground' : 'border-transparent text-muted hover:text-foreground',
          )}
        >
          {tab.label}
        </button>
      ))}
    </div>
  )
}

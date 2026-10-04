import { clsx } from 'clsx'
import NextLink from 'next/link'

interface TabsProps<T extends string> {
  label: string
  tabs: { id: T; label: string }[]
  value: T
  onChange: (id: T) => void
}

const tabClass = (active: boolean) => clsx(
  '-mb-px border-b-2 pb-2.5 text-sm transition-colors',
  active ? 'border-accent text-foreground' : 'border-transparent text-muted hover:text-foreground',
)

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
          className={tabClass(tab.id === value)}
        >
          {tab.label}
        </button>
      ))}
    </div>
  )
}

/** The same row as links between pages, the current one marked. */
export function LinkTabs({ label, tabs }: { label: string; tabs: { href: string; label: string; active: boolean }[] }) {
  return (
    <nav aria-label={label} className="flex gap-6 border-b">
      {tabs.map(tab => (
        <NextLink key={tab.href} href={tab.href} aria-current={tab.active ? 'page' : undefined} className={tabClass(tab.active)}>
          {tab.label}
        </NextLink>
      ))}
    </nav>
  )
}

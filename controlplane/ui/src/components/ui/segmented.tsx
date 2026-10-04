import type { ReactNode } from 'react'
import { clsx } from 'clsx'
import { controlHeight } from '@/components/ui/field'
import type { FieldSize } from '@/components/ui/field'

interface SegmentedProps<T extends string> {
  label: string
  /** With an icon, the option shows only the icon and its label names it to a screen reader and on hover. */
  options: { value: T; label: string; icon?: ReactNode }[]
  value: T
  onChange: (value: T) => void
  disabled?: boolean
  /** The height of the controls beside it, as in field.ts. */
  size?: FieldSize
}

/** A few exclusive choices side by side, one always chosen: a control of field.ts cut into segments. */
export function Segmented<T extends string>({ label, options, value, onChange, disabled, size = 'default' }: SegmentedProps<T>) {
  return (
    <div
      role="group"
      aria-label={label}
      className={clsx('inline-flex flex-none gap-0.5 rounded border border-border-control bg-surface p-0.5', controlHeight[size])}
    >
      {options.map(option => (
        <button
          key={option.value}
          type="button"
          disabled={disabled}
          aria-pressed={option.value === value}
          aria-label={option.icon ? option.label : undefined}
          title={option.icon ? option.label : undefined}
          onClick={() => onChange(option.value)}
          className={clsx(
            'inline-flex items-center justify-center whitespace-nowrap rounded-sm font-medium transition-colors disabled:cursor-not-allowed disabled:opacity-disabled',
            size === 'sm' ? 'text-xs' : 'text-sm',
            option.icon ? 'px-2' : size === 'sm' ? 'px-2.5' : 'px-3.5',
            option.value === value ? 'bg-primary text-primary-foreground' : 'text-muted enabled:hover:bg-hover enabled:hover:text-foreground',
          )}
        >
          {option.icon ?? option.label}
        </button>
      ))}
    </div>
  )
}

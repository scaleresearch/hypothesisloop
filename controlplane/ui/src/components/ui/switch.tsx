'use client'

import { Switch as HeadlessSwitch } from '@headlessui/react'
import { clsx } from 'clsx'

interface SwitchProps {
  checked: boolean
  onChange: (checked: boolean) => void
  /** What the switch turns on and off, for a reader who does not see the row it sits in. */
  label: string
  disabled?: boolean
  className?: string
}

export function Switch({ checked, onChange, label, disabled, className }: SwitchProps) {
  return (
    <HeadlessSwitch
      checked={checked}
      onChange={onChange}
      disabled={disabled}
      aria-label={label}
      className={clsx(
        'group inline-flex h-5 w-9 flex-none cursor-pointer items-center rounded-full border border-border-control bg-surface p-0.5 transition-colors',
        'data-[checked]:border-primary data-[checked]:bg-primary data-[disabled]:cursor-not-allowed data-[disabled]:opacity-disabled',
        className,
      )}
    >
      <span
        aria-hidden="true"
        className="h-3.5 w-3.5 rounded-full bg-muted transition-transform group-data-[checked]:translate-x-4 group-data-[checked]:bg-primary-foreground"
      />
    </HeadlessSwitch>
  )
}

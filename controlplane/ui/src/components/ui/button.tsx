import type { ButtonHTMLAttributes } from 'react'
import { clsx } from 'clsx'

type Variant = 'default' | 'primary' | 'danger' | 'link'
type Size = 'default' | 'sm'

interface ButtonProps extends ButtonHTMLAttributes<HTMLButtonElement> {
  variant?: Variant
  size?: Size
}

const variantClass: Record<Variant, string> = {
  default: 'wa-btn',
  primary: 'wa-btn wa-btn-primary',
  danger: 'wa-btn wa-btn-danger',
  link: 'wa-btn-link',
}

/** Shared button used for toolbar actions (Refresh, New, Cancel, Save…). */
export function Button({ variant = 'default', size = 'default', className, ...props }: ButtonProps) {
  return (
    <button
      className={clsx(variantClass[variant], size === 'sm' && variant !== 'link' && 'wa-btn-sm', className)}
      {...props}
    />
  )
}

interface ChipProps extends ButtonHTMLAttributes<HTMLButtonElement> {
  active?: boolean
}

/** Toggle-style filter chip, e.g. status filter row. */
export function Chip({ active, className, ...props }: ChipProps) {
  return <button className={clsx('wa-chip', active && 'active', className)} {...props} />
}

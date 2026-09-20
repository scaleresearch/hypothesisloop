import type { ReactNode } from 'react'
import { Description, Field, Label } from '@headlessui/react'
import { clsx } from 'clsx'
import { InlineError } from '@/components/ui/status-message'

interface FormFieldProps {
  label: ReactNode
  hint?: ReactNode
  error?: ReactNode
  /** Label and control on one line, for a filter row. */
  inline?: boolean
  className?: string
  /** One control from this directory; it is named by the label and described by the hint. */
  children: ReactNode
}

export function FormField({ label, hint, error, inline, className, children }: FormFieldProps) {
  return (
    <Field className={clsx(inline ? 'flex items-center gap-2' : 'grid content-start gap-1', className)}>
      <Label className="text-2xs font-semibold uppercase tracking-wider text-muted">{label}</Label>
      {children}
      {hint && <Description className="text-xs text-subtle">{hint}</Description>}
      {error && <InlineError>{error}</InlineError>}
    </Field>
  )
}

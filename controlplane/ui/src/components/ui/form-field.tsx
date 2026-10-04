import type { ReactNode } from 'react'
import { Description, Field, Label } from '@headlessui/react'
import { clsx } from 'clsx'
import { InlineError } from '@/components/ui/status-message'
import { Eyebrow, Text, captionClass } from '@/components/ui/text'

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
    <Field className={clsx(inline ? 'flex items-center gap-2' : 'grid content-start gap-1.5', className)}>
      <Label className={clsx(captionClass, 'text-muted')}>{label}</Label>
      {children}
      {hint && <Description className="text-xs text-subtle">{hint}</Description>}
      {error && <InlineError>{error}</InlineError>}
    </Field>
  )
}

/** The caption and hint of FormField over several controls, which one label cannot name. */
export function FieldGroup({ label, hint, className, children }: { label: ReactNode; hint?: ReactNode; className?: string; children: ReactNode }) {
  return (
    <div role="group" aria-label={typeof label === 'string' ? label : undefined} className={clsx('grid content-start gap-1.5', className)}>
      <Eyebrow>{label}</Eyebrow>
      <div>{children}</div>
      {hint && <Text variant="hint">{hint}</Text>}
    </div>
  )
}

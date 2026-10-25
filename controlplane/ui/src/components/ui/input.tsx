import { forwardRef } from 'react'
import type { InputHTMLAttributes, TextareaHTMLAttributes } from 'react'
import { Input as HeadlessInput, Textarea as HeadlessTextarea } from '@headlessui/react'
import { clsx } from 'clsx'
import { fieldClass } from '@/components/ui/field'
import type { FieldSize } from '@/components/ui/field'

interface FieldProps {
  fieldSize?: FieldSize
  mono?: boolean
  invalid?: boolean
  /** No box of its own, for an input drawn inside another control. */
  bare?: boolean
}

/** Inside a FormField it takes the field's label, hint and error by itself. */
export const Input = forwardRef<HTMLInputElement, InputHTMLAttributes<HTMLInputElement> & FieldProps>(
  function Input({ fieldSize, mono, bare, className, ...props }, ref) {
    return <HeadlessInput ref={ref} className={clsx(fieldClass(fieldSize, mono, bare), className)} {...props} />
  },
)

export const Textarea = forwardRef<HTMLTextAreaElement, TextareaHTMLAttributes<HTMLTextAreaElement> & FieldProps>(
  function Textarea({ fieldSize, mono, bare, className, ...props }, ref) {
    return <HeadlessTextarea ref={ref} className={clsx(fieldClass(fieldSize, mono, bare), !bare && 'resize-y', className)} {...props} />
  },
)

import type { SelectHTMLAttributes } from 'react'
import { Select as HeadlessSelect } from '@headlessui/react'
import { clsx } from 'clsx'
import { fieldClass } from '@/components/ui/field'
import type { FieldSize } from '@/components/ui/field'

interface SelectProps extends SelectHTMLAttributes<HTMLSelectElement> {
  fieldSize?: FieldSize
  mono?: boolean
  invalid?: boolean
}

/** A native select for a short fixed list; a list that grows takes SearchableSelect. */
export function Select({ fieldSize, mono, className, ...props }: SelectProps) {
  return <HeadlessSelect className={clsx(fieldClass(fieldSize, mono), 'cursor-pointer', className)} {...props} />
}

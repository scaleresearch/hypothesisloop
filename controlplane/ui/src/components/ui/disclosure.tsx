'use client'

import type { ReactNode } from 'react'
import { Disclosure as HeadlessDisclosure, DisclosureButton, DisclosurePanel } from '@headlessui/react'
import { ChevronRight } from 'lucide-react'
import { clsx } from 'clsx'

/** A part of a form or a record that stays folded until asked for: advanced settings, a log. */
export function Disclosure({ label, defaultOpen, className, children }: {
  label: ReactNode
  defaultOpen?: boolean
  className?: string
  children: ReactNode
}) {
  return (
    <HeadlessDisclosure as="div" defaultOpen={defaultOpen} className={clsx('grid justify-items-start gap-2', className)}>
      <DisclosureButton className="group inline-flex items-center gap-1 rounded-sm text-xs font-medium text-muted transition-colors hover:text-foreground">
        <ChevronRight aria-hidden="true" size={13} className="transition-transform group-data-[open]:rotate-90" />
        {label}
      </DisclosureButton>
      <DisclosurePanel className="w-full">{children}</DisclosurePanel>
    </HeadlessDisclosure>
  )
}

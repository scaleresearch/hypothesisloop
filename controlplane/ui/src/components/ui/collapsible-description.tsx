import { Disclosure, DisclosureButton } from '@headlessui/react'
import { clsx } from 'clsx'

const DEFAULT_COLLAPSE_LENGTH = 220

/** Long free-text blocks (experiment descriptions, etc.) collapse behind a "Show more" toggle. */
export function CollapsibleDescription({ text, collapseLength = DEFAULT_COLLAPSE_LENGTH, className }: {
  text: string
  collapseLength?: number
  className?: string
}) {
  if (text.length <= collapseLength) {
    return <p className={clsx('whitespace-pre-wrap text-sm text-subtle', className)}>{text}</p>
  }
  return (
    <Disclosure as="div" className={className}>
      {({ open }: { open: boolean }) => (
        <div onClick={e => e.stopPropagation()}>
          <p className="mb-1 whitespace-pre-wrap text-sm text-subtle">
            {open ? text : text.slice(0, collapseLength).trimEnd() + '…'}
          </p>
          <DisclosureButton className="text-xs text-accent hover:underline">
            {open ? 'Show less ▲' : 'Show more ▼'}
          </DisclosureButton>
        </div>
      )}
    </Disclosure>
  )
}

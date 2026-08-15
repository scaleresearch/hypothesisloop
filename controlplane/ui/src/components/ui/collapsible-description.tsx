import { Disclosure, DisclosureButton } from '@headlessui/react'

const DEFAULT_COLLAPSE_LENGTH = 220

/** Long free-text blocks (experiment descriptions, etc.) collapse behind a "Show more" toggle. */
export function CollapsibleDescription({ text, collapseLength = DEFAULT_COLLAPSE_LENGTH, style }: {
  text: string
  collapseLength?: number
  style?: React.CSSProperties
}) {
  if (text.length <= collapseLength) {
    return <p className="text-dim" style={{ fontSize: 13, whiteSpace: 'pre-wrap', ...style }}>{text}</p>
  }
  return (
    <Disclosure as="div" style={style}>
      {({ open }: { open: boolean }) => (
        <div onClick={e => e.stopPropagation()}>
          <p className="text-dim" style={{ fontSize: 13, marginBottom: 4, whiteSpace: 'pre-wrap' }}>
            {open ? text : text.slice(0, collapseLength).trimEnd() + '…'}
          </p>
          <DisclosureButton className="text-link" style={{ fontSize: 12, background: 'none', border: 'none', cursor: 'pointer', padding: 0 }}>
            {open ? 'Show less ▲' : 'Show more ▼'}
          </DisclosureButton>
        </div>
      )}
    </Disclosure>
  )
}

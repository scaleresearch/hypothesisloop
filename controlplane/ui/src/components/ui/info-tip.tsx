import { Info } from 'lucide-react'

/** The detail behind a one-line hint, on hover and to a screen reader. */
export function InfoTip({ children }: { children: string }) {
  return (
    <span role="img" aria-label={children} title={children} className="ml-1 inline-flex cursor-help align-text-bottom text-subtle">
      <Info size={13} aria-hidden="true" />
    </span>
  )
}

import type { ReactNode } from 'react'
import { Button } from '@/components/ui/button'
import { CopyButton } from '@/components/ui/copy-button'
import { Eyebrow } from '@/components/ui/text'

/**
 * A token on screen, with a copy control. It lives in the state of whoever rendered this and
 * nowhere else, so dismissing it or leaving the page takes it off the screen. `note` says whether
 * it can be had again, and `dismiss` words the button to match.
 */
export function Secret({ label, value, note, dismiss, onDismiss }: {
  label: string
  value: string
  note: ReactNode
  dismiss: string
  onDismiss: () => void
}) {
  return (
    <div className="grid gap-2 rounded border border-primary bg-surface-inset px-3.5 py-3" role="status">
      <Eyebrow>{label}</Eyebrow>
      <pre className="select-all whitespace-pre-wrap break-all font-mono text-xs" data-testid="secret">{value}</pre>
      <p className="text-xs text-muted">{note}</p>
      <div className="flex flex-wrap items-center gap-2">
        <CopyButton value={value} variant="primary" />
        <Button type="button" size="sm" onClick={onDismiss}>{dismiss}</Button>
      </div>
    </div>
  )
}

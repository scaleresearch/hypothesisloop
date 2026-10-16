'use client'

import { useState, type ReactNode } from 'react'
import { Dialog as HeadlessDialog, DialogBackdrop, DialogPanel, DialogTitle } from '@headlessui/react'
import { clsx } from 'clsx'
import { errorMessage } from '@/lib/errors'
import { Button } from '@/components/ui/button'
import { InlineError } from '@/components/ui/status-message'

interface DialogProps {
  open: boolean
  onClose: () => void
  title: ReactNode
  size?: 'default' | 'sm' | 'wide'
  children: ReactNode
}

/** A modal: focus stays inside, Escape and a click outside close it. */
export function Dialog({ open, onClose, title, size = 'default', children }: DialogProps) {
  return (
    <HeadlessDialog open={open} onClose={onClose} className="relative z-50">
      <DialogBackdrop className="fixed inset-0 bg-scrim" />
      <div className="fixed inset-0 flex items-center justify-center p-4">
        <DialogPanel
          className={clsx(
            'max-h-full w-full animate-rise overflow-y-auto rounded-lg border bg-surface-raised p-6 shadow-lg',
            size === 'sm' ? 'max-w-md' : size === 'wide' ? 'max-w-7xl' : 'max-w-2xl',
          )}
        >
          <DialogTitle className="mb-4 text-xl font-normal">{title}</DialogTitle>
          {children}
        </DialogPanel>
      </div>
    </HeadlessDialog>
  )
}

interface ConfirmDialogProps {
  open: boolean
  title: ReactNode
  /** What happens on confirm, in a sentence. */
  children: ReactNode
  confirmLabel: string
  cancelLabel?: string
  /** The confirm button is the danger one unless the action is harmless. */
  variant?: 'danger' | 'primary'
  /** The action. The dialog stays open until it settles, and shows its failure. */
  onConfirm: () => Promise<void>
  /** Called on cancel and once the action succeeded. */
  onClose: () => void
}

/** Asks before an action, runs it and closes once it is done; a failed one leaves the dialog open with the reason. */
export function ConfirmDialog({ open, title, children, confirmLabel, cancelLabel = 'Cancel', variant = 'danger', onConfirm, onClose }: ConfirmDialogProps) {
  const [pending, setPending] = useState(false)
  const [error, setError] = useState('')

  async function confirm() {
    setPending(true)
    setError('')
    try {
      await onConfirm()
    } catch (e) {
      setError(errorMessage(e))
      return
    } finally {
      setPending(false)
    }
    onClose()
  }

  function cancel() {
    if (pending) return
    setError('')
    onClose()
  }

  return (
    <Dialog open={open} onClose={cancel} title={title} size="sm">
      <p className="text-sm text-muted">{children}</p>
      {error && <InlineError className="mt-3 block">{error}</InlineError>}
      <div className="mt-5 flex justify-end gap-2">
        <Button type="button" onClick={cancel} disabled={pending}>{cancelLabel}</Button>
        <Button type="button" variant={variant} onClick={confirm} loading={pending}>{confirmLabel}</Button>
      </div>
    </Dialog>
  )
}

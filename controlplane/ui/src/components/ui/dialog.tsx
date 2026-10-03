'use client'

import type { ReactNode } from 'react'
import { Dialog as HeadlessDialog, DialogBackdrop, DialogPanel, DialogTitle } from '@headlessui/react'
import { clsx } from 'clsx'
import { Button } from '@/components/ui/button'

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
            'max-h-[calc(100vh-2rem)] w-full animate-rise overflow-y-auto rounded-lg border bg-surface-raised p-6 shadow-lg',
            size === 'sm' ? 'max-w-md' : size === 'wide' ? 'max-w-[90vw]' : 'max-w-2xl',
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
  onConfirm: () => void
  onCancel: () => void
}

export function ConfirmDialog({ open, title, children, confirmLabel, cancelLabel = 'Cancel', variant = 'danger', onConfirm, onCancel }: ConfirmDialogProps) {
  return (
    <Dialog open={open} onClose={onCancel} title={title} size="sm">
      <p className="text-sm text-muted">{children}</p>
      <div className="mt-5 flex justify-end gap-2">
        <Button type="button" onClick={onCancel}>{cancelLabel}</Button>
        <Button type="button" variant={variant} onClick={onConfirm}>{confirmLabel}</Button>
      </div>
    </Dialog>
  )
}

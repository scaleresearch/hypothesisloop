import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { useState } from 'react'
import { describe, expect, it, vi } from 'vitest'
import { ApiError } from '@/lib/api'
import { ConfirmDialog } from '@/components/ui/dialog'

function Asking({ action }: { action: () => Promise<void> }) {
  const [open, setOpen] = useState(true)
  return (
    <>
      <output>{open ? 'open' : 'closed'}</output>
      <ConfirmDialog open={open} title="Remove it?" confirmLabel="Remove" onConfirm={action} onClose={() => setOpen(false)}>
        It goes for good.
      </ConfirmDialog>
    </>
  )
}

function deferred() {
  let settle!: { resolve: () => void; reject: (e: unknown) => void }
  const promise = new Promise<void>((resolve, reject) => { settle = { resolve, reject } })
  return { promise, ...settle }
}

describe('a confirmation', () => {
  it('stays open with a busy confirm button until the action is done, then closes', async () => {
    const action = deferred()
    render(<Asking action={() => action.promise} />)
    await userEvent.click(screen.getByRole('button', { name: 'Remove' }))
    const confirm = screen.getByRole('button', { name: 'Remove' }) as HTMLButtonElement
    expect(confirm.disabled).toBe(true)
    expect(confirm.getAttribute('aria-busy')).toBe('true')
    expect((screen.getByRole('button', { name: 'Cancel' }) as HTMLButtonElement).disabled).toBe(true)
    expect(screen.getByRole('dialog')).toBeTruthy()
    action.resolve()
    await waitFor(() => expect(screen.getByText('closed')).toBeTruthy())
  })

  it('shows a failure inside and stays open, to try again or cancel', async () => {
    const action = vi.fn().mockRejectedValueOnce(new ApiError(409, 'the agent has a job running')).mockResolvedValueOnce(undefined)
    render(<Asking action={action} />)
    await userEvent.click(screen.getByRole('button', { name: 'Remove' }))
    const alert = await screen.findByRole('alert')
    expect(alert.textContent).toBe('the agent has a job running')
    expect(screen.getByRole('dialog').contains(alert)).toBe(true)
    expect(screen.getByText('open')).toBeTruthy()
    await userEvent.click(screen.getByRole('button', { name: 'Remove' }))
    await waitFor(() => expect(screen.getByText('closed')).toBeTruthy())
    expect(action).toHaveBeenCalledTimes(2)
  })

  it('cancels without running the action', async () => {
    const action = vi.fn()
    render(<Asking action={action} />)
    await userEvent.click(screen.getByRole('button', { name: 'Cancel' }))
    expect(screen.getByText('closed')).toBeTruthy()
    expect(action).not.toHaveBeenCalled()
  })
})

import { render, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'
import { ConfirmDialog, Dialog } from '@/components/ui/dialog'

describe('a dialog', () => {
  it('is named by its title and closes on Escape', async () => {
    const onClose = vi.fn()
    render(<Dialog open onClose={onClose} title="Add an idea"><button>Inside</button></Dialog>)
    expect(screen.getByRole('dialog', { name: 'Add an idea' })).toBeTruthy()
    await userEvent.keyboard('{Escape}')
    expect(onClose).toHaveBeenCalledOnce()
  })

  it('renders nothing while closed', () => {
    render(<Dialog open={false} onClose={() => {}} title="Hidden">body</Dialog>)
    expect(screen.queryByRole('dialog')).toBeNull()
  })

  it('keeps focus inside while open', async () => {
    render(<><button>Outside</button><Dialog open onClose={() => {}} title="Trap"><button>First</button><button>Second</button></Dialog></>)
    const dialog = screen.getByRole('dialog')
    await userEvent.tab()
    await userEvent.tab()
    await userEvent.tab()
    expect(dialog.contains(document.activeElement)).toBe(true)
  })
})

describe('a confirmation', () => {
  function ask(onConfirm = vi.fn(), onCancel = vi.fn()) {
    render(
      <ConfirmDialog open title="Revoke the key?" confirmLabel="Revoke" onConfirm={onConfirm} onCancel={onCancel}>
        It stops working at once.
      </ConfirmDialog>,
    )
    return { onConfirm, onCancel, dialog: within(screen.getByRole('dialog', { name: 'Revoke the key?' })) }
  }

  it('says what will happen and goes ahead on its confirm button', async () => {
    const { onConfirm, onCancel, dialog } = ask()
    expect(dialog.getByText('It stops working at once.')).toBeTruthy()
    await userEvent.click(dialog.getByRole('button', { name: 'Revoke' }))
    expect(onConfirm).toHaveBeenCalledOnce()
    expect(onCancel).not.toHaveBeenCalled()
  })

  it('does nothing on Cancel or Escape but say so', async () => {
    const { onConfirm, onCancel, dialog } = ask()
    await userEvent.click(dialog.getByRole('button', { name: 'Cancel' }))
    await userEvent.keyboard('{Escape}')
    expect(onCancel).toHaveBeenCalledTimes(2)
    expect(onConfirm).not.toHaveBeenCalled()
  })
})

import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { closePlatformExperiment } from '@/lib/api/platform-experiments'
import { CloseExperiment } from '@/components/close-experiment'
import type { PlatformExperiment } from '@/types/api'

vi.mock('@/lib/api/platform-experiments', () => ({ closePlatformExperiment: vi.fn() }))

const pe = { id: 'pe-1', name: 'Cosine', status: 'running' } as PlatformExperiment

beforeEach(() => { vi.mocked(closePlatformExperiment).mockReset().mockResolvedValue(pe) })

describe('closing an experiment', () => {
  it('asks first, says what happens, and closes nothing until confirmed', async () => {
    const onClosed = vi.fn()
    render(<CloseExperiment pe={pe} onClosed={onClosed} />)
    await userEvent.click(screen.getByRole('button', { name: 'Close experiment' }))
    expect(screen.getByText(/agents wind down and every running job stops/)).toBeTruthy()
    expect(closePlatformExperiment).not.toHaveBeenCalled()
    await userEvent.click(screen.getAllByRole('button', { name: 'Close experiment' }).at(-1)!)
    await waitFor(() => expect(onClosed).toHaveBeenCalled())
    expect(closePlatformExperiment).toHaveBeenCalledWith('pe-1')
  })

  it('leaves it open and shows the refusal', async () => {
    vi.mocked(closePlatformExperiment).mockRejectedValue(new Error('already closed'))
    const onClosed = vi.fn()
    render(<CloseExperiment pe={pe} onClosed={onClosed} />)
    await userEvent.click(screen.getByRole('button', { name: 'Close experiment' }))
    await userEvent.click(screen.getAllByRole('button', { name: 'Close experiment' }).at(-1)!)
    expect((await screen.findByRole('alert')).textContent).toBe('already closed')
    expect(onClosed).not.toHaveBeenCalled()
  })

  it('does not close on cancel', async () => {
    render(<CloseExperiment pe={pe} onClosed={vi.fn()} />)
    await userEvent.click(screen.getByRole('button', { name: 'Close experiment' }))
    await userEvent.click(screen.getByRole('button', { name: 'Cancel' }))
    expect(closePlatformExperiment).not.toHaveBeenCalled()
  })
})

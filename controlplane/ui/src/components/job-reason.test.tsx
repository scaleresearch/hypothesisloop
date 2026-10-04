import { fireEvent, render, screen } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import { JobReason } from '@/components/job-reason'

describe('the reason a job ended', () => {
  it('says the sentence and folds the last output behind a toggle that a row click does not see', () => {
    const rowClick = vi.fn()
    render(
      <div onClick={rowClick}>
        <JobReason reason={'job_failed: exited with code 1\nlast output:\nTraceback (most recent call last)\nValueError: bad shape'} />
      </div>,
    )
    expect(screen.getByText(/exited with code 1/)).toBeTruthy()
    expect(screen.queryByText(/ValueError: bad shape/)).toBeNull()
    fireEvent.click(screen.getByRole('button', { name: 'Last output' }))
    expect(screen.getByText(/ValueError: bad shape/)).toBeTruthy()
    expect(screen.queryByText(/^last output:/)).toBeNull()
    expect(rowClick).not.toHaveBeenCalled()
  })

  it('shows no toggle for a reason without output, and opens it when asked to', () => {
    const { unmount } = render(<JobReason reason="stuck_pending: no node fits" />)
    expect(screen.queryByRole('button', { name: 'Last output' })).toBeNull()
    unmount()
    render(<JobReason reason={'job_failed: exited\nlast output:\nboom'} outputOpen />)
    expect(screen.getByText('boom')).toBeTruthy()
  })
})

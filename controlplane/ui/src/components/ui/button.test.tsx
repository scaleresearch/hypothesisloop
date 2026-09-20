import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'
import { Button, Chip, RowButton } from '@/components/ui/button'

describe('a button', () => {
  it('runs its action on a click', async () => {
    const onClick = vi.fn()
    render(<Button onClick={onClick}>Save</Button>)
    await userEvent.click(screen.getByRole('button', { name: 'Save' }))
    expect(onClick).toHaveBeenCalledOnce()
  })

  it('refuses clicks and says it is busy while loading', async () => {
    const onClick = vi.fn()
    render(<Button loading onClick={onClick}>Save</Button>)
    const button = screen.getByRole('button', { name: 'Save' })
    expect(button).toHaveProperty('disabled', true)
    expect(button.getAttribute('aria-busy')).toBe('true')
    await userEvent.click(button)
    expect(onClick).not.toHaveBeenCalled()
  })

  it('submits the form it sits in unless told it is a plain button', async () => {
    const onSubmit = vi.fn((e: React.FormEvent) => e.preventDefault())
    render(<form onSubmit={onSubmit}><Button>Go</Button><Button type="button">Other</Button></form>)
    await userEvent.click(screen.getByRole('button', { name: 'Other' }))
    expect(onSubmit).not.toHaveBeenCalled()
    await userEvent.click(screen.getByRole('button', { name: 'Go' }))
    expect(onSubmit).toHaveBeenCalledOnce()
  })
})

describe('a chip', () => {
  it('says whether its filter is in force', () => {
    render(<><Chip active>Running</Chip><Chip>Failed</Chip></>)
    expect(screen.getByRole('button', { name: 'Running' }).getAttribute('aria-pressed')).toBe('true')
    expect(screen.getByRole('button', { name: 'Failed' }).getAttribute('aria-pressed')).toBe('false')
  })
})

describe('a row button', () => {
  it('hands its element to a ref, so a list can scroll to the selected row', () => {
    let row: HTMLButtonElement | null = null
    render(<RowButton selected ref={el => { row = el }}>First</RowButton>)
    expect(row).toBe(screen.getByRole('button', { name: 'First' }))
  })
})

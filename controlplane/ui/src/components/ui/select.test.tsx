import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { useState } from 'react'
import { describe, expect, it } from 'vitest'
import { Dialog } from '@/components/ui/dialog'
import { Select } from '@/components/ui/select'

const options = [
  { value: 'a', label: 'Alpha' },
  { value: 'b', label: 'Beta' },
  { value: 'c', label: 'Gamma', disabled: true },
  { value: 'd', label: 'Delta' },
]

function Picker({ initial = 'a' }: { initial?: string }) {
  const [value, setValue] = useState(initial)
  return (
    <>
      <Select aria-label="Letter" placeholder="Pick a letter" prefix="Sort" value={value} onChange={setValue} options={options} />
      <output>{value}</output>
    </>
  )
}

function chosen() {
  return screen.getByRole('status').textContent
}

describe('a select', () => {
  it('is a button named by its label that shows the value and opens a listbox', async () => {
    render(<Picker />)
    const field = screen.getByLabelText('Letter')
    expect(field.tagName).toBe('BUTTON')
    expect(field.textContent).toBe('Sort Alpha')
    expect(field.getAttribute('aria-haspopup')).toBe('listbox')
    expect(field.getAttribute('aria-labelledby')!.split(' ')).toContain(field.id)
    expect(screen.queryByRole('listbox')).toBeNull()

    await userEvent.click(field)
    expect(field.getAttribute('aria-expanded')).toBe('true')
    expect(screen.getAllByRole('option').map(o => o.textContent)).toEqual(['Alpha', 'Beta', 'Gamma', 'Delta'])
    expect(screen.getByRole('option', { name: 'Alpha' }).getAttribute('aria-selected')).toBe('true')
  })

  it('takes a choice by a click, closes and keeps focus on the field', async () => {
    render(<Picker />)
    const field = screen.getByLabelText('Letter')
    await userEvent.click(field)
    await userEvent.click(screen.getByRole('option', { name: 'Beta' }))
    expect(chosen()).toBe('b')
    expect(screen.queryByRole('listbox')).toBeNull()
    expect(document.activeElement).toBe(field)
  })

  it('is operated from the keyboard alone, and steps over a disabled option', async () => {
    render(<Picker initial="b" />)
    await userEvent.tab()
    const field = screen.getByLabelText('Letter')
    expect(document.activeElement).toBe(field)
    await userEvent.keyboard('{ArrowDown}')
    await screen.findByRole('listbox')
    await userEvent.keyboard('{ArrowDown}{Enter}')
    expect(chosen()).toBe('d')
    expect(document.activeElement).toBe(field)

    await userEvent.keyboard(' ')
    await screen.findByRole('listbox')
    await userEvent.keyboard('{Escape}')
    expect(screen.queryByRole('listbox')).toBeNull()
    expect(chosen()).toBe('d')
  })

  it('refuses a disabled option', async () => {
    render(<Picker />)
    await userEvent.click(screen.getByLabelText('Letter'))
    await userEvent.click(screen.getByRole('option', { name: 'Gamma' }))
    expect(chosen()).toBe('a')
  })

  it('shows the placeholder while the value is none of the options', () => {
    render(<Picker initial="" />)
    expect(screen.getByLabelText('Letter').textContent).toBe('Sort Pick a letter')
  })

  it('opens its list outside a dialog\'s panel, so the panel cannot clip it', async () => {
    render(<Dialog open onClose={() => {}} title="Pick"><Picker /></Dialog>)
    await userEvent.click(screen.getByLabelText('Letter'))
    const list = await screen.findByRole('listbox')
    expect(screen.getByLabelText('Letter').closest('[id^="headlessui-dialog-panel"]')).not.toBeNull()
    expect(list.closest('[id^="headlessui-dialog-panel"]')).toBeNull()
    await userEvent.click(screen.getByRole('option', { name: 'Delta' }))
    expect(chosen()).toBe('d')
    expect(screen.getByRole('dialog')).toBeTruthy()
  })
})

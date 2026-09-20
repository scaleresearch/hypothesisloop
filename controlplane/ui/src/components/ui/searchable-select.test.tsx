import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { useState } from 'react'
import { describe, expect, it } from 'vitest'
import { SearchableSelect } from '@/components/ui/searchable-select'

const options = [{ value: 'a', label: 'Alpha' }, { value: 'b', label: 'Beta' }, { value: 'c', label: 'Gamma' }]

function Picker() {
  const [value, setValue] = useState('a')
  return (
    <>
      <SearchableSelect options={options} value={value} onChange={setValue} hideAllOption />
      <output>{value}</output>
    </>
  )
}

function chosen() {
  return screen.getByRole('status').textContent
}

describe('a searchable select', () => {
  it('keeps focus after a choice and opens again on a click', async () => {
    render(<Picker />)
    const field = screen.getByRole('combobox')
    await userEvent.click(field)
    await userEvent.click(await screen.findByRole('option', { name: 'Beta' }))
    expect(chosen()).toBe('b')
    expect(screen.queryByRole('option')).toBeNull()
    expect(document.activeElement).toBe(field)

    await userEvent.click(field)
    await userEvent.click(await screen.findByRole('option', { name: 'Gamma' }))
    expect(chosen()).toBe('c')
    expect(document.activeElement).toBe(field)
  })

  it('is operated from the keyboard alone, choice after choice', async () => {
    render(<Picker />)
    await userEvent.tab()
    const field = screen.getByRole('combobox')
    expect(document.activeElement).toBe(field)
    await screen.findByRole('option', { name: 'Beta' })
    await userEvent.keyboard('{ArrowDown}{Enter}')
    expect(chosen()).toBe('b')
    expect(screen.queryByRole('option')).toBeNull()
    expect(document.activeElement).toBe(field)

    await userEvent.keyboard('{ArrowDown}')
    await screen.findByRole('option', { name: 'Gamma' })
    await userEvent.keyboard('{ArrowDown}{Enter}')
    expect(chosen()).toBe('c')

    await userEvent.keyboard('{ArrowDown}')
    await screen.findByRole('option', { name: 'Alpha' })
    await userEvent.keyboard('{Escape}')
    expect(screen.queryByRole('option')).toBeNull()
    expect(chosen()).toBe('c')
    expect(document.activeElement).toBe(field)
  })

  it('narrows the list as the person types', async () => {
    render(<Picker />)
    await userEvent.click(screen.getByRole('combobox'))
    await userEvent.clear(screen.getByRole('combobox'))
    await userEvent.type(screen.getByRole('combobox'), 'gam')
    expect((await screen.findAllByRole('option')).map(o => o.textContent)).toEqual(['Gamma'])
  })
})

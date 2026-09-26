import { render, screen } from '@testing-library/react'
import { choose } from '@/test-select'
import userEvent from '@testing-library/user-event'
import { useState } from 'react'
import { describe, expect, it } from 'vitest'
import { Checkbox } from '@/components/ui/checkbox'
import { FieldGroup, FormField } from '@/components/ui/form-field'
import { Input, Textarea } from '@/components/ui/input'
import { Select } from '@/components/ui/select'

describe('a form field', () => {
  it('names its input by the label and describes it by the hint', () => {
    render(<FormField label="Budget" hint="In accelerator hours"><Input defaultValue="10" /></FormField>)
    const input = screen.getByLabelText('Budget')
    expect(input).toHaveProperty('value', '10')
    const described = input.getAttribute('aria-describedby')!
    expect(document.getElementById(described)!.textContent).toBe('In accelerator hours')
  })

  it('keeps the control beside its label, so the label leads to it', () => {
    render(<FormField label="Model"><Input /></FormField>)
    expect(screen.getByText('Model').parentElement!.querySelector('input')).toBe(screen.getByLabelText('Model'))
  })

  it('announces its error', () => {
    render(<FormField label="Name" error="already taken"><Input /></FormField>)
    expect(screen.getByRole('alert').textContent).toBe('already taken')
  })

  it('names a textarea and a select the same way', async () => {
    function Direction() {
      const [value, setValue] = useState('maximize')
      return (
        <FormField label="Direction">
          <Select value={value} onChange={setValue} options={[{ value: 'maximize', label: 'maximize' }, { value: 'minimize', label: 'minimize' }]} />
        </FormField>
      )
    }
    render(<><FormField label="Description"><Textarea /></FormField><Direction /></>)
    await userEvent.type(screen.getByLabelText('Description'), 'two\nlines')
    expect(screen.getByLabelText('Description')).toHaveProperty('value', 'two\nlines')
    await choose(screen.getByLabelText('Direction'), 'minimize')
    expect(screen.getByLabelText('Direction')).toHaveProperty('textContent', 'minimize')
  })
})

describe('a field group', () => {
  it('names several controls as one group', () => {
    render(<FieldGroup label="Run by" hint="Who starts the agent"><button>Me</button></FieldGroup>)
    expect(screen.getByRole('group', { name: 'Run by' }).textContent).toContain('Who starts the agent')
  })
})

describe('a checkbox', () => {
  it('is named by its label and toggles on a click of it', async () => {
    function Toggle() {
      const [on, setOn] = useState(false)
      return <Checkbox pill label="loss" checked={on} onChange={() => setOn(!on)} />
    }
    render(<Toggle />)
    const box = screen.getByRole('checkbox', { name: 'loss' })
    expect(box).toHaveProperty('checked', false)
    await userEvent.click(screen.getByText('loss'))
    expect(box).toHaveProperty('checked', true)
  })
})

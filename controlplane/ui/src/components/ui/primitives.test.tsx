import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { useState } from 'react'
import { describe, expect, it, vi } from 'vitest'
import { Badge } from '@/components/ui/badge'
import { CopyButton } from '@/components/ui/copy-button'
import { MetricBar } from '@/components/ui/metric-bar'
import { PageHeader } from '@/components/ui/page-header'
import { EmptyState, ErrorMessage, Loading } from '@/components/ui/status-message'
import { Segmented, Tabs } from '@/components/ui/tabs'
import { statusColor, statusTone, tint } from '@/lib/colors'

describe('a badge', () => {
  it('shows the state it is given and takes its tone from it, whatever the case', () => {
    render(<Badge status="RUNNING" />)
    expect(screen.getByText('RUNNING').className).toContain('text-success')
    expect(statusTone('running')).toBe('success')
    expect(statusTone('Refuted')).toBe('danger')
  })

  it('is neutral for a state it does not know', () => {
    expect(statusTone('brand-new-state')).toBe('neutral')
    expect(statusTone(undefined)).toBe('neutral')
  })
})

describe('theme colours in script', () => {
  it('are the tokens of the stylesheet, so charts follow the theme', () => {
    expect(statusColor('FAILED')).toBe('rgb(var(--danger))')
    expect(tint(statusColor('FAILED'), 0.1)).toBe('color-mix(in srgb, rgb(var(--danger)) 10%, transparent)')
  })
})

describe('status messages', () => {
  it('announces an error and keeps the class the browser suite finds it by', () => {
    render(<ErrorMessage>Could not load</ErrorMessage>)
    const alert = screen.getByRole('alert')
    expect(alert.textContent).toBe('Could not load')
    expect(alert.classList.contains('error-banner')).toBe(true)
  })

  it('names what is loading', () => {
    render(<Loading text="Loading keys…" />)
    expect(screen.getByRole('status', { name: 'Loading keys…' })).toBeTruthy()
  })

  it('offers the action of an empty state under its text', () => {
    render(<EmptyState action={<button>Create one</button>}>Nothing yet.</EmptyState>)
    expect(screen.getByText('Nothing yet.')).toBeTruthy()
    expect(screen.getByRole('button', { name: 'Create one' })).toBeTruthy()
  })
})

describe('a page header', () => {
  it('has the title as the page heading and a way back', async () => {
    const onClick = vi.fn()
    render(<PageHeader title="Run 12" description="A job" back={{ label: 'Jobs', onClick }} actions={<button>Cancel</button>} />)
    expect(screen.getByRole('heading', { level: 1, name: 'Run 12' })).toBeTruthy()
    await userEvent.click(screen.getByRole('button', { name: '← Jobs' }))
    expect(onClick).toHaveBeenCalledOnce()
  })
})

describe('a metric bar', () => {
  it('reports the share used, capped at the whole', () => {
    render(<><MetricBar value={30} max={120} /><MetricBar value={500} max={100} /><MetricBar value={1} max={0} /></>)
    expect(screen.getAllByRole('progressbar').map(b => b.getAttribute('aria-valuenow'))).toEqual(['25', '100', '0'])
  })
})

describe('tabs', () => {
  it('mark the chosen tab and report a click on another', async () => {
    function Steps() {
      const [step, setStep] = useState<'one' | 'two'>('one')
      return <Tabs label="Steps" tabs={[{ id: 'one', label: '1. Essentials' }, { id: 'two', label: '2. Settings' }]} value={step} onChange={setStep} />
    }
    render(<Steps />)
    expect(screen.getByRole('tab', { name: '1. Essentials' }).getAttribute('aria-selected')).toBe('true')
    await userEvent.click(screen.getByRole('tab', { name: '2. Settings' }))
    expect(screen.getByRole('tab', { name: '2. Settings' }).getAttribute('aria-selected')).toBe('true')
    expect(screen.getByRole('tab', { name: '1. Essentials' }).getAttribute('aria-selected')).toBe('false')
  })
})

describe('a segmented choice', () => {
  const options = [{ value: 'mixed', label: 'Anyone' }, { value: 'human_only', label: 'Humans' }]

  it('marks the chosen option and reports another', async () => {
    const onChange = vi.fn()
    render(<Segmented label="Who can submit" options={options} value="mixed" onChange={onChange} />)
    expect(screen.getByRole('button', { name: 'Anyone' }).getAttribute('aria-pressed')).toBe('true')
    await userEvent.click(screen.getByRole('button', { name: 'Humans' }))
    expect(onChange).toHaveBeenCalledExactlyOnceWith('human_only')
  })

  it('changes nothing while disabled', async () => {
    const onChange = vi.fn()
    render(<Segmented label="Who can submit" options={options} value="mixed" onChange={onChange} disabled />)
    await userEvent.click(screen.getByRole('button', { name: 'Humans' }))
    expect(onChange).not.toHaveBeenCalled()
  })
})

describe('a copy button', () => {
  it('copies the value and says so', async () => {
    const user = userEvent.setup()
    render(<CopyButton value="hls_token" />)
    await user.click(screen.getByRole('button', { name: 'Copy' }))
    expect(await screen.findByRole('button', { name: 'Copied' })).toBeTruthy()
    expect(await navigator.clipboard.readText()).toBe('hls_token')
  })

  it('says why when the clipboard refuses', async () => {
    vi.stubGlobal('navigator', { clipboard: { writeText: () => Promise.reject(new Error('denied')) } })
    render(<CopyButton value="hls_token" />)
    await userEvent.click(screen.getByRole('button', { name: 'Copy' }))
    expect((await screen.findByRole('alert')).textContent).toBe('Copy failed, select the text instead: denied')
  })
})

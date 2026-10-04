import { render, screen } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import ExperimentLayout from '@/app/(app)/experiments/[experimentId]/layout'
import { Page } from '@/components/ui/page'

const navigation = vi.hoisted(() => ({ pathname: '/experiments/pe-x' }))

vi.mock('next/navigation', () => ({ usePathname: () => navigation.pathname }))

function show(pathname: string) {
  navigation.pathname = pathname
  return render(<ExperimentLayout params={{ experimentId: 'pe-x' }}><Page title="Vision benchmark" /></ExperimentLayout>)
}

describe('the tabs of an experiment page', () => {
  it('link the overview, hypotheses, jobs and agents, the overview current on the experiment itself', () => {
    show('/experiments/pe-x')
    const tabs = screen.getAllByRole('link')
    expect(tabs.map(t => [t.textContent, t.getAttribute('href')])).toEqual([
      ['Overview', '/experiments/pe-x'],
      ['Hypotheses', '/experiments/pe-x/hypotheses'],
      ['Jobs', '/experiments/pe-x/jobs'],
      ['Agents', '/experiments/pe-x/agents'],
    ])
    expect(screen.getByRole('link', { name: 'Overview' }).getAttribute('aria-current')).toBe('page')
  })

  it('mark the tab that lists what a nested page shows', () => {
    show('/experiments/pe-x/jobs/job-1')
    expect(screen.getByRole('link', { name: 'Jobs' }).getAttribute('aria-current')).toBe('page')
    expect(screen.getByRole('link', { name: 'Overview' }).getAttribute('aria-current')).toBeNull()
  })
})

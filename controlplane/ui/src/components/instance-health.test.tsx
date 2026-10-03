import { act, render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { InstanceHealth } from '@/components/instance-health'
import type { AgentInstance } from '@/types'

const NOW = new Date('2026-10-05T12:00:00Z')
const ago = (seconds: number) => new Date(NOW.getTime() - seconds * 1000).toISOString()

function instance(over: Partial<AgentInstance> = {}): AgentInstance {
  return { state: 'running', restarts: 0, stopped: false, ...over }
}

beforeEach(() => {
  vi.useFakeTimers({ shouldAdvanceTime: true })
  vi.setSystemTime(NOW)
})

afterEach(() => {
  vi.useRealTimers()
})

describe('the health of a hosted machine', () => {
  it('shows its state and when it last made progress, and no restarts while it has had none', () => {
    render(<InstanceHealth instance={instance({ progress_at: ago(120), seen_at: ago(900) })} />)
    expect(screen.getByText('running')).toBeTruthy()
    expect(screen.getByText('last progress 2m ago')).toBeTruthy()
    expect(screen.queryByText(/restarted/)).toBeNull()
    expect(screen.queryByText(/Replaced|Stopped for good/)).toBeNull()
    expect(screen.queryByRole('button', { name: 'Exit log' })).toBeNull()
  })

  it.each(['requested', 'starting'] as const)('says no progress was seen yet while it is %s', state => {
    render(<InstanceHealth instance={instance({ state })} />)
    expect(screen.getByText(state)).toBeTruthy()
    expect(screen.getByText('no progress seen yet')).toBeTruthy()
  })

  it('says nothing of progress while it waits for the start, when no machine exists', () => {
    render(<InstanceHealth instance={instance({ state: 'waiting' })} />)
    expect(screen.getByText('waiting')).toBeTruthy()
    expect(screen.queryByText('no progress seen yet')).toBeNull()
  })

  it('keeps the age of the last progress up with the clock between reads', () => {
    render(<InstanceHealth instance={instance({ progress_at: ago(30) })} />)
    expect(screen.getByText('last progress 30s ago')).toBeTruthy()
    act(() => { vi.advanceTimersByTime(90_000) })
    expect(screen.getByText('last progress 2m ago')).toBeTruthy()
  })

  it('says quietly why and when a machine was replaced, with its log behind a disclosure', async () => {
    render(<InstanceHealth instance={instance({
      restarts: 1, progress_at: ago(10), end_reason: 'silent', exited_at: ago(600), exit_log: 'Traceback: out of memory',
    })} />)
    expect(screen.getByText('running')).toBeTruthy()
    expect(screen.getByText('restarted once')).toBeTruthy()
    expect(screen.getByText('Replaced: the machine before this one ended 10m ago, it ran and made no progress.')).toBeTruthy()
    expect(screen.queryByText('Traceback: out of memory')).toBeNull()
    await userEvent.click(screen.getByRole('button', { name: 'Exit log' }))
    expect(screen.getByText('Traceback: out of memory')).toBeTruthy()
  })

  it.each([
    ['exited', 'its command ended'],
    ['gone', 'its provider no longer lists it'],
    ['never_seen', 'no machine came to be'],
    ['still_starting', 'it did not come up in time'],
    ['silent', 'it ran and made no progress'],
  ] as const)('words the end of a machine that %s', (end_reason, words) => {
    render(<InstanceHealth instance={instance({ restarts: 2, end_reason, exited_at: ago(60) })} />)
    expect(screen.getByText(new RegExp(`ended 1m ago, ${words}\\.$`))).toBeTruthy()
  })

  it.each([
    ['expired', 'its provider ended it at the provider’s own lifetime'],
    ['progress_unreadable', 'its output could not be read for as long as progress is waited for'],
  ] as const)('says a machine that ended as %s counts as no restart', (end_reason, words) => {
    render(<InstanceHealth instance={instance({ end_reason, exited_at: ago(60) })} />)
    expect(screen.getByText(`Replaced: the machine before this one ended 1m ago, ${words}, which counts as no restart.`)).toBeTruthy()
    expect(screen.queryByText(/restarted/)).toBeNull()
  })

  it('says a machine with no restart left is stopped for good, and shows no progress age', () => {
    render(<InstanceHealth instance={instance({
      state: 'stopped', stopped: true, restarts: 3, progress_at: ago(4000), end_reason: 'exited', exited_at: ago(3600), exit_log: 'exit 1',
    })} />)
    expect(screen.getByText('stopped')).toBeTruthy()
    expect(screen.getByText('restarted 3 times')).toBeTruthy()
    expect(screen.getByText('Stopped for good: its last machine ended 1h ago, its command ended.')).toBeTruthy()
    expect(screen.queryByText(/last progress/)).toBeNull()
  })

  it('says a machine the platform released was no failure, with which end of the run it was', () => {
    const why = 'the platform released the machine on purpose: its agent finished'
    render(<InstanceHealth instance={instance({ state: 'stopped', end_reason: 'released', exited_at: ago(300), exit_log: why })} />)
    expect(screen.getByText('Released: its run was over and its machine ended 5m ago, the platform took it down.')).toBeTruthy()
    expect(screen.getByText(why)).toBeTruthy()
    expect(screen.queryByRole('button', { name: 'Exit log' })).toBeNull()
  })

  it('shows a reason it does not know as the API sent it', () => {
    render(<InstanceHealth instance={instance({ restarts: 1, end_reason: 'evicted' as never, exited_at: ago(60) })} />)
    expect(screen.getByText(/ended 1m ago, evicted\.$/)).toBeTruthy()
  })
})

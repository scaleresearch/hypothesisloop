import { act, render, screen } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { InstanceHealth } from '@/components/instance-health'
import type { AgentInstance } from '@/types/api'

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
  it('keeps the age of the machine up with the clock between reads', () => {
    render(<InstanceHealth instance={instance({ asked_at: ago(120), seen_at: ago(30) })} />)
    expect(screen.getByText('first reported 30s ago')).toBeTruthy()
    act(() => { vi.advanceTimersByTime(90_000) })
    expect(screen.getByText('first reported 2m ago')).toBeTruthy()
  })

  it('says a machine with no restart left is stopped for good, and shows no age of its machine', () => {
    render(<InstanceHealth instance={instance({
      state: 'stopped', stopped: true, restarts: 3, progress_at: ago(4000), end_reason: 'exited', exited_at: ago(3600), exit_log: 'exit 1',
    })} />)
    expect(screen.getByText('stopped')).toBeTruthy()
    expect(screen.getByText('restarted 3 times')).toBeTruthy()
    expect(screen.getByText('Stopped for good: its last machine ended 1h ago, its command ended.')).toBeTruthy()
    expect(screen.queryByText(/first reported/)).toBeNull()
  })
})

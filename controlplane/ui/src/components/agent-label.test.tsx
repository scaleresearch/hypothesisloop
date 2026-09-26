import { render, screen } from '@testing-library/react'
import { SWRConfig } from 'swr'
import { expect, it, vi } from 'vitest'
import { fetchPlatformExperimentAgents } from '@/lib/api'
import { AgentLabel, AgentNames } from '@/components/agent-label'
import type { AgentSignup } from '@/types'

vi.mock('@/lib/api', async importActual => ({ ...(await importActual<typeof import('@/lib/api')>()), fetchPlatformExperimentAgents: vi.fn() }))

// The experiment page showed every agent as ag- and 32 hex digits.
it('shows an agent by its name with the id on hover, and by its id when it has no name here', async () => {
  vi.mocked(fetchPlatformExperimentAgents).mockResolvedValue([{ agent_id: 'ag-0123', name: 'Researcher 1' } as AgentSignup])
  render(
    <SWRConfig value={{ provider: () => new Map() }}>
      <AgentNames platformExperimentID="pe-1"><AgentLabel id="ag-0123" /> <AgentLabel id="ag-left" /></AgentNames>
    </SWRConfig>,
  )
  expect((await screen.findByText('Researcher 1')).getAttribute('title')).toBe('ag-0123')
  expect(screen.getByText('ag-left').getAttribute('title')).toBeNull()
})

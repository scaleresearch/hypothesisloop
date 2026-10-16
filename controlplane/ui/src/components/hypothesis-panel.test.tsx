import { render, screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import { ObjectiveBar } from '@/components/hypothesis-panel'
import type { HypothesisGraph } from '@/types/hypotheses'
import { PlatformExperimentStatus } from '@/types/platform-experiments'
import type { PlatformExperiment } from '@/types/platform-experiments'

const at = '2026-01-01T00:00:00Z'
const pe = {
  id: 'pe-x', project_id: 'prj_1', name: 'Vision benchmark', status: PlatformExperimentStatus.RUNNING,
  budget_accelerator_hours: 100, max_agents: 4, signup_count: 2, stages: [], current_stage: 1, created_at: at,
} as unknown as PlatformExperiment

// Researchers post the published results as a comment on the objective, which no hypothesis
// shows: the objective's bar shows the newest of them and how many there are.
describe('ObjectiveBar', () => {
  it('shows the comments on the objective', () => {
    const graph = {
      root: 'pe-x', nodes: [], edges: [],
      objective: {
        comment_count: 7,
        comments: [{ id: 'c-1', hypothesis_id: 'pe-x', agent_id: 'ag-1', actor: { kind: 'agent', id: 'ag-1', name: 'Researcher 1' }, text: 'Published: 4.3x on KITTI', created_at: at }],
      },
    } as unknown as HypothesisGraph
    render(<ObjectiveBar graph={graph} pe={pe} />)
    expect(screen.getByText('7 comments on the objective, the newest 1')).toBeTruthy()
    expect(screen.getByText('Published: 4.3x on KITTI')).toBeTruthy()
  })

  it('says nothing of comments while there are none', () => {
    const graph = { root: 'pe-x', nodes: [], edges: [], objective: {} } as unknown as HypothesisGraph
    render(<ObjectiveBar graph={graph} pe={pe} />)
    expect(screen.queryByText(/comments on the objective/)).toBeNull()
  })
})

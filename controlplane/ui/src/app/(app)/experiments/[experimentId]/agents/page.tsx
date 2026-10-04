'use client'

import { AgentsList } from '@/components/agents-list'

export default function ExperimentAgentsPage({ params }: { params: { experimentId: string } }) {
  return <AgentsList experimentID={params.experimentId} />
}

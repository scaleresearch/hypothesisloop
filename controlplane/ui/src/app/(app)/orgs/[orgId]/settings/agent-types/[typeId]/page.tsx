'use client'

import { AgentTypeDetail } from '@/components/agent-types/detail'

export default function AgentTypePage({ params }: { params: { typeId: string } }) {
  return <AgentTypeDetail typeID={decodeURIComponent(params.typeId)} />
}

'use client'

import { Suspense } from 'react'
import { Loading } from '@/components/ui/status-message'
import { ExperimentHypotheses } from '@/components/hypotheses-workspace'

export default function HypothesesPage({ params }: { params: { experimentId: string } }) {
  return (
    <Suspense fallback={<Loading />}>
      <ExperimentHypotheses peID={params.experimentId} />
    </Suspense>
  )
}

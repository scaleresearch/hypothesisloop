'use client'

import { JobsList } from '@/components/jobs-list'

export default function ExperimentJobsPage({ params }: { params: { experimentId: string } }) {
  return <JobsList experimentID={params.experimentId} />
}

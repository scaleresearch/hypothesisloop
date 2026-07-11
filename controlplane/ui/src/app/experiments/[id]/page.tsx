import { redirect } from 'next/navigation'

export default function ExperimentDetailRedirect({ params }: { params: { id: string } }) {
  // Legacy URLs: /experiments/:id → /jobs/:id
  // Can't use params in server redirect easily, so just redirect to jobs list
  redirect('/jobs')
}

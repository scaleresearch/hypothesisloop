'use client'

import { Suspense, useEffect } from 'react'
import { useRouter, useSearchParams } from 'next/navigation'
import { CONSOLE_SECTION_PARAM, isSection } from '@/lib/legacy-routes'
import { sectionPath } from '@/lib/routes'
import { useTenant } from '@/lib/tenant'
import { Loading } from '@/components/ui/status-message'

/**
 * Where sign-in lands: the experiments of the caller's home project, or another of its sections an
 * old link named, with the view state that link carried.
 */
export default function ConsolePage() {
  return (
    <Suspense fallback={<Loading />}>
      <ToHome />
    </Suspense>
  )
}

function ToHome() {
  const { project } = useTenant()
  const router = useRouter()
  const query = useSearchParams()
  useEffect(() => {
    const rest = new URLSearchParams(query)
    const named = rest.get(CONSOLE_SECTION_PARAM)
    const section = isSection(named) ? named : 'experiments'
    rest.delete(CONSOLE_SECTION_PARAM)
    const qs = rest.toString()
    router.replace(sectionPath(section, project) + (qs ? `?${qs}` : ''))
  }, [project, query, router])
  return <Loading />
}

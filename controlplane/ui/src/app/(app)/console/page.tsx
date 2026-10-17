'use client'

import { useEffect } from 'react'
import { useRouter } from 'next/navigation'
import { projectPath } from '@/lib/routes'
import { useTenant } from '@/lib/tenant'
import { Loading } from '@/components/ui/status-message'

/** Where sign-in lands: the experiments of the caller's home project. */
export default function ConsolePage() {
  const { project } = useTenant()
  const router = useRouter()
  useEffect(() => {
    router.replace(projectPath(project.id, 'experiments'))
  }, [project, router])
  return <Loading />
}

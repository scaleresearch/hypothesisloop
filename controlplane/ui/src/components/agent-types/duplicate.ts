import { useState } from 'react'
import { useRouter } from 'next/navigation'
import { useSWRConfig } from 'swr'
import { createAgentType } from '@/lib/api'
import type { CreateAgentTypeRequest } from '@/lib/api'
import { agentTypesPath } from '@/lib/routes'

/** Makes an agent type of the org, a copy or a new one, and opens it in the editor. */
export function useCreateAgentType(orgID: string) {
  const router = useRouter()
  const { mutate } = useSWRConfig()
  const [busy, setBusy] = useState<string | null>(null)
  const [error, setError] = useState('')

  async function create(req: CreateAgentTypeRequest, key: string): Promise<boolean> {
    setBusy(key)
    setError('')
    try {
      const made = await createAgentType(orgID, req)
      await mutate(['agent-types', orgID])
      router.push(agentTypesPath(orgID, made.id))
      return true
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
      return false
    } finally {
      setBusy(null)
    }
  }

  return { create, busy, error }
}

import { useContext } from 'react'
import useSWR from 'swr'
import { fetchResourceCapacity } from '@/lib/api/clusters'
import { keys } from '@/lib/keys'
import { TenantContext } from '@/lib/tenant'

/** What the org's jobs can be placed on now, with the rate of each type. Read once, not polled. */
export function useCapacity() {
  const orgID = useContext(TenantContext)?.org.id
  return useSWR(orgID ? keys.resourceCapacity(orgID) : null, () => fetchResourceCapacity(orgID!))
}

/** The accelerator budgets are counted in, as the platform names it, undefined until it is read. */
export function useBudgetReference(): string | undefined {
  return useCapacity().data?.budget_reference || undefined
}

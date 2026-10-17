import { render } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import { resolveTenant, TenantContext } from '@/lib/tenant'
import ConsolePage from '@/app/(app)/console/page'
import type { Me } from '@/types/api'

const replace = vi.hoisted(() => vi.fn())

vi.mock('next/navigation', () => ({
  useRouter: () => ({ replace }),
}))

const at = '2026-01-01T00:00:00Z'
const me: Me = {
  kind: 'user',
  user_id: 'u1',
  orgs: [{ id: 'org_a', name: 'Ada', role: 'owner', created_at: at, actions: ['edit', 'create_api_key'] }],
  projects: [{ id: 'prj_1', org_id: 'org_a', name: 'default', created_at: at }],
}

describe('the console', () => {
  it('opens the experiments of the home project', async () => {
    const resolved = resolveTenant(me, '')
    if (resolved.state !== 'selected') throw new Error('no tenant')
    render(<TenantContext.Provider value={resolved.tenant}><ConsolePage /></TenantContext.Provider>)
    await vi.waitFor(() => expect(replace).toHaveBeenCalledWith('/projects/prj_1/experiments'))
  })
})

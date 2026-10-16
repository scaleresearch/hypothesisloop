import { render } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { resolveTenant, TenantContext } from '@/lib/tenant'
import ConsolePage from '@/app/(app)/console/page'
import type { Me } from '@/types/accounts'

const navigation = vi.hoisted(() => ({ replace: vi.fn(), search: '' }))

vi.mock('next/navigation', () => ({
  useRouter: () => ({ replace: navigation.replace }),
  useSearchParams: () => new URLSearchParams(navigation.search),
}))

const at = '2026-01-01T00:00:00Z'
const me: Me = {
  kind: 'user',
  user_id: 'u1',
  orgs: [{ id: 'org_a', name: 'Ada', role: 'owner', created_at: at }],
  projects: [{ id: 'prj_1', org_id: 'org_a', name: 'default', created_at: at }],
}

function landOn(search: string) {
  navigation.search = search
  const resolved = resolveTenant(me, '')
  if (resolved.state !== 'selected') throw new Error('no tenant')
  render(<TenantContext.Provider value={resolved.tenant}><ConsolePage /></TenantContext.Provider>)
}

beforeEach(() => navigation.replace.mockReset())

describe('a bare section, sent through the console', () => {
  it.each([
    ['', '/projects/prj_1/experiments'],
    ['to=experiments', '/projects/prj_1/experiments'],
    ['to=hypotheses', '/projects/prj_1/hypotheses'],
    ['to=jobs', '/projects/prj_1/jobs'],
    ['to=statistics', '/projects/prj_1/statistics'],
    ['to=clusters', '/orgs/org_a/clusters'],
    ['to=settings', '/orgs/org_a/settings'],
    ['to=hypotheses&status=open', '/projects/prj_1/hypotheses?status=open'],
  ])('?%s opens %s in the home project, where the nav links', async (search, path) => {
    landOn(search)
    await vi.waitFor(() => expect(navigation.replace).toHaveBeenCalledWith(path))
  })
})

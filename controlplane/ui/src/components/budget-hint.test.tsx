import { render, screen } from '@testing-library/react'
import { SWRConfig } from 'swr'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { fetchResourceCapacity } from '@/lib/api/clusters'
import { resolveTenant, TenantContext } from '@/lib/tenant'
import { BudgetHint, acceleratorRates, budgetExplained, budgetOn } from '@/components/budget-hint'
import type { LiveCapacity, Me } from '@/types/api'

vi.mock('@/lib/api/clusters', () => ({ fetchResourceCapacity: vi.fn() }))

const at = '2026-01-01T00:00:00Z'
const me: Me = {
  kind: 'user', user_id: 'u1', name: 'Ada',
  orgs: [{ id: 'org_a', name: 'Lab', role: 'owner', created_at: at, actions: [] }],
  projects: [{ id: 'prj_1', org_id: 'org_a', name: 'default', created_at: at }],
}

const accelerator = (type: string, rate: number, kind: 'accelerator' | 'cpu' = 'accelerator') =>
  ({ accelerator_type: type, kind, acch_per_hour: rate, available: 1, total: 1, per_node_available: 1 })

const CAPACITY = {
  budget_reference: 'H100',
  clusters: [
    { cluster_name: 'a', hosts: 1, multi_node: false, accelerators: [accelerator('nvidia.com/gpu.product=NVIDIA-GeForce-RTX-4090', 0.25), accelerator('hypothesisloop.dev/cpu=core', 1, 'cpu')] },
    { cluster_name: 'b', hosts: 1, multi_node: false, accelerators: [accelerator('nvidia.com/gpu.product=NVIDIA-H100-80GB-HBM3', 1), accelerator('nvidia.com/gpu.product=NVIDIA-GeForce-RTX-4090', 0.25)] },
  ],
} as unknown as LiveCapacity

function show(node: React.ReactNode) {
  const resolved = resolveTenant(me, '')
  if (resolved.state !== 'selected') throw new Error('no tenant')
  return render(<SWRConfig value={{ provider: () => new Map() }}><TenantContext.Provider value={resolved.tenant}>{node}</TenantContext.Provider></SWRConfig>)
}

beforeEach(() => {
  vi.mocked(fetchResourceCapacity).mockResolvedValue(CAPACITY)
})

describe('what a budget is counted in', () => {
  it('lists each accelerator type once with its rate, dearest first, and no CPU', () => {
    expect(acceleratorRates(CAPACITY).map(r => `${r.label} ${r.rate}`)).toEqual(['NVIDIA-H100-80GB-HBM3 1', 'NVIDIA-GeForce-RTX-4090 0.25'])
    expect(acceleratorRates(undefined)).toEqual([])
  })

  it('says what a budget buys on one type', () => {
    expect(budgetOn(10, { type: 't', label: 'RTX 4090', rate: 0.25 })).toBe('About 40 hours on RTX 4090, which counts 0.25 per hour.')
    expect(budgetOn(1, { type: 't', label: 'T4', rate: 0.3 })).toBe('About 3.3 hours on T4, which counts 0.3 per hour.')
  })

  it('names the reference accelerator, and core-hours for a run on CPU', () => {
    const rates = acceleratorRates(CAPACITY)
    expect(budgetExplained(undefined, 'H100', rates)).toBe('Budgets are counted in H100-hours. An hour on another type counts at its rate: NVIDIA-H100-80GB-HBM3 1, NVIDIA-GeForce-RTX-4090 0.25. Jobs on CPU count CPU core-hours. Memory and storage spend nothing.')
    expect(budgetExplained('accelerator', 'H100', [])).toBe('Budgets are counted in H100-hours. An hour on another type counts at its rate. Memory and storage spend nothing.')
    expect(budgetExplained('cpu', 'H100', rates)).toBe('Counted in CPU core-hours: a core for an hour counts 1. Memory and storage spend nothing.')
  })

  it('converts the budget for the chosen type as it is typed', async () => {
    show(<BudgetHint budget="10" acceleratorType="nvidia.com/gpu.product=NVIDIA-GeForce-RTX-4090" />)
    expect(await screen.findByText('About 40 hours on NVIDIA-GeForce-RTX-4090, which counts 0.25 per hour.')).toBeTruthy()
    expect(screen.getByRole('img', { name: /^Budgets are counted in H100-hours\./ })).toBeTruthy()
  })

  it('shows only the tip with no type chosen', async () => {
    show(<BudgetHint budget="10" />)
    expect(await screen.findByRole('img', { name: /^Budgets are counted in H100-hours\./ })).toBeTruthy()
    expect(screen.queryByText(/^About /)).toBeNull()
  })
})

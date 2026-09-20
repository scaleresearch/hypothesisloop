import { render, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { SWRConfig } from 'swr'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { fetchClusterSettings, fetchOrgClusters, putClusterSettings, registerCluster } from '@/lib/api'
import { resolveTenant, TenantContext } from '@/lib/tenant'
import { OrgClusters } from '@/components/org-clusters'
import type { ClusterSettings, Me, OrgCluster } from '@/types'

vi.mock('@/lib/api', () => ({
  fetchClusterSettings: vi.fn(), fetchOrgClusters: vi.fn(), putClusterSettings: vi.fn(), registerCluster: vi.fn(),
}))

const at = '2026-01-01T00:00:00Z'
const me: Me = {
  kind: 'user',
  user_id: 'u1',
  name: 'Ada',
  orgs: [{ id: 'org_a', name: 'Ada', role: 'owner', created_at: at }],
  projects: [{ id: 'prj_1', org_id: 'org_a', name: 'default', created_at: at }],
}

let clusters: OrgCluster[]
let settings: ClusterSettings

function show({ manage = true, as = me }: { manage?: boolean; as?: Me } = {}) {
  const resolved = resolveTenant(as, '')
  if (resolved.state !== 'selected') throw new Error('no tenant')
  return render(
    <SWRConfig value={{ provider: () => new Map() }}>
      <TenantContext.Provider value={resolved.tenant}><OrgClusters manage={manage} /></TenantContext.Provider>
    </SWRConfig>,
  )
}

beforeEach(() => {
  clusters = [
    { name: 'lab', org_id: 'org_a', shared: false, cluster_id: 'cid-1', created_at: at, owned: true, connected: true },
    { name: 'new', org_id: 'org_a', shared: false, created_at: at, owned: true, connected: false },
  ]
  settings = { cluster_id: 'cid-1', scale_up_timeout_seconds: 600, max_speculative_accelerators: null, stuck_pending_timeout_seconds: null }
  vi.mocked(fetchOrgClusters).mockImplementation(async () => [...clusters])
  vi.mocked(fetchClusterSettings).mockImplementation(async () => ({ ...settings }))
  vi.mocked(putClusterSettings).mockImplementation(async (id, next) => (settings = { cluster_id: id, ...next }))
  vi.mocked(registerCluster).mockImplementation(async (orgID, name) => {
    if (!clusters.some(c => c.name === name)) {
      clusters = [...clusters, { name, org_id: orgID, shared: false, created_at: at, owned: true, connected: false }]
    }
    return { name, api_url: 'http://api.cluster.test', metrics_url: 'http://metrics.cluster.test' }
  })
})

describe('the org\'s clusters', () => {
  it('lists each with whether its agent is connected', async () => {
    show()
    const lab = (await screen.findByRole('cell', { name: /^lab/ })).closest('tr')!
    expect(within(lab).getByText('Connected')).toBeTruthy()
    const fresh = screen.getByRole('cell', { name: 'new' }).closest('tr')!
    expect(within(fresh).getByText('Disconnected')).toBeTruthy()
  })

  it('offers settings for a cluster that has reported its id, and says why another has none', async () => {
    show()
    expect(await screen.findByText(/scale-up timeout: 600s · speculative cap: default · stuck-pending timeout: default/)).toBeTruthy()
    expect(screen.getByText('available once its agent has connected')).toBeTruthy()
    expect(fetchClusterSettings).toHaveBeenCalledExactlyOnceWith('cid-1')
  })

  it('saves the stuck-pending timeout with the rest of the row, empty fields as the default', async () => {
    show()
    await userEvent.click(await screen.findByRole('button', { name: 'Edit' }))
    await userEvent.type(screen.getByLabelText('stuck-pending timeout'), '900')
    await userEvent.click(screen.getByRole('button', { name: 'Save' }))
    expect(putClusterSettings).toHaveBeenCalledExactlyOnceWith('cid-1', {
      scale_up_timeout_seconds: 600, max_speculative_accelerators: null, stuck_pending_timeout_seconds: 900,
    })
    expect(await screen.findByText(/stuck-pending timeout: 900s/)).toBeTruthy()
  })

  it('clears an override by emptying its field', async () => {
    show()
    await userEvent.click(await screen.findByRole('button', { name: 'Edit' }))
    await userEvent.clear(screen.getByLabelText('scale-up timeout'))
    await userEvent.click(screen.getByRole('button', { name: 'Save' }))
    expect(putClusterSettings).toHaveBeenCalledExactlyOnceWith('cid-1', {
      scale_up_timeout_seconds: null, max_speculative_accelerators: null, stuck_pending_timeout_seconds: null,
    })
  })

  it('refuses a value that is no whole number, without asking the API', async () => {
    show()
    await userEvent.click(await screen.findByRole('button', { name: 'Edit' }))
    await userEvent.type(screen.getByLabelText('speculative cap'), 'many')
    await userEvent.click(screen.getByRole('button', { name: 'Save' }))
    expect(await screen.findByText('speculative cap must be a whole number, or empty for the default')).toBeTruthy()
    expect(putClusterSettings).not.toHaveBeenCalled()
  })

  it('shows the API\'s refusal and stays in the editor', async () => {
    vi.mocked(putClusterSettings).mockRejectedValue(new Error('scale_up_timeout_seconds must be under 1800'))
    show()
    await userEvent.click(await screen.findByRole('button', { name: 'Edit' }))
    await userEvent.click(screen.getByRole('button', { name: 'Save' }))
    expect(await screen.findByText('scale_up_timeout_seconds must be under 1800')).toBeTruthy()
    expect(screen.getByRole('button', { name: 'Save' })).toBeTruthy()
  })
})

describe('a member who is no owner', () => {
  const member: Me = { ...me, orgs: [{ ...me.orgs[0], role: 'member' }] }

  it('is told who sets a cluster\'s settings, and is asked for none the API would refuse', async () => {
    show({ as: member })
    const lab = (await screen.findByRole('cell', { name: /^lab/ })).closest('tr')!
    expect(within(lab).getByText('set by an owner of Ada')).toBeTruthy()
    expect(screen.queryByRole('button', { name: 'Edit' })).toBeNull()
    expect(screen.queryByRole('button', { name: 'Add cluster' })).toBeNull()
    expect(fetchClusterSettings).not.toHaveBeenCalled()
  })
})

describe('a cluster name the API refuses', () => {
  it('stays in the field with the refusal beside it, and is cleared once one is taken', async () => {
    vi.mocked(registerCluster).mockRejectedValueOnce(new Error('cluster name "Lab K8s" must be lowercase letters, digits and dashes'))
    show()
    const field = await screen.findByLabelText('Cluster name') as HTMLInputElement
    await userEvent.type(field, 'Lab K8s')
    await userEvent.click(screen.getByRole('button', { name: 'Add cluster' }))
    expect(await screen.findByText(/must be lowercase letters, digits and dashes/)).toBeTruthy()
    expect(field.value).toBe('Lab K8s')

    await userEvent.clear(field)
    await userEvent.type(field, 'lab-k8s')
    await userEvent.click(screen.getByRole('button', { name: 'Add cluster' }))
    expect(await screen.findByTestId('secret')).toBeTruthy()
    expect(field.value).toBe('')
  })
})

describe('a cluster the platform shares with the org', () => {
  beforeEach(() => {
    clusters = [...clusters, { name: 'platform-gpu', shared: true, created_at: at, owned: false, connected: true }]
  })

  it('is listed with its status, as the platform\'s, with nothing to change', async () => {
    show()
    const row = (await screen.findByRole('cell', { name: /^platform-gpu/ })).closest('tr')!
    expect(within(row).getByText('Connected')).toBeTruthy()
    expect(within(row).getByText('the platform')).toBeTruthy()
    expect(within(row).getByText('shared with every org')).toBeTruthy()
    expect(within(row).getByText('set by the platform')).toBeTruthy()
    expect(within(row).queryByRole('button')).toBeNull()
    expect(fetchClusterSettings).toHaveBeenCalledExactlyOnceWith('cid-1')
  })
})

describe('a cluster\'s start command', () => {
  it('is shown again on request: registering an owned name answers the same', async () => {
    show()
    const lab = (await screen.findByRole('cell', { name: /^lab/ })).closest('tr')!
    await userEvent.click(within(lab).getByRole('button', { name: 'Show start command' }))
    expect(registerCluster).toHaveBeenCalledExactlyOnceWith('org_a', 'lab')
    expect((await screen.findByTestId('secret')).textContent).toContain(`hl cluster token 'lab' --org 'org_a')" make cluster-agent-up CLUSTER='lab'`)
    expect(screen.getByText(/brings the\s+same command back/)).toBeTruthy()
    expect(document.body.textContent).not.toMatch(/never again|only time/)
  })

  it('is not offered where clusters are only shown, nor to a member', async () => {
    const shown = show({ manage: false })
    await screen.findByRole('cell', { name: /^lab/ })
    expect(screen.queryByRole('button', { name: 'Show start command' })).toBeNull()
    expect(screen.queryByLabelText('Cluster name')).toBeNull()
    shown.unmount()

    show({ as: { ...me, orgs: [{ ...me.orgs[0], role: 'member' }] } })
    await screen.findByRole('cell', { name: /^lab/ })
    expect(screen.queryByRole('button', { name: 'Show start command' })).toBeNull()
    expect(screen.queryByLabelText('Cluster name')).toBeNull()
  })
})

describe('adding a cluster', () => {
  it('shows the command hl cluster add prints, which carries no token, and lists the cluster', async () => {
    show()
    await userEvent.type(screen.getByLabelText('Cluster name'), 'edge')
    await userEvent.click(screen.getByRole('button', { name: 'Add cluster' }))
    expect(registerCluster).toHaveBeenCalledExactlyOnceWith('org_a', 'edge')
    expect((await screen.findByTestId('secret')).textContent).toBe(
      `RUNTIME_TOKEN="$(hl cluster token 'edge' --org 'org_a')" make cluster-agent-up CLUSTER='edge' API_URL='http://api.cluster.test' METRICS_URL='http://metrics.cluster.test'`,
    )
    expect(document.body.textContent).not.toContain('hls_')
    expect(await screen.findByRole('cell', { name: 'edge' })).toBeTruthy()
    await userEvent.click(screen.getByRole('button', { name: 'Hide' }))
    expect(screen.queryByTestId('secret')).toBeNull()
  })

  it('shows the API\'s refusal, as for a name another org owns', async () => {
    vi.mocked(registerCluster).mockRejectedValue(new Error('cluster name edge is taken'))
    show()
    await userEvent.type(screen.getByLabelText('Cluster name'), 'edge')
    await userEvent.click(screen.getByRole('button', { name: 'Add cluster' }))
    expect(await screen.findByText('cluster name edge is taken')).toBeTruthy()
    expect(screen.queryByTestId('secret')).toBeNull()
  })
})

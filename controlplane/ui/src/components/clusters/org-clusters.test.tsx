import { fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { SWRConfig } from 'swr'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { fetchOrgClusters, registerCluster, removeCluster, setClusterEnabled, setClusterOrder, updateCluster } from '@/lib/api/clusters'
import { resolveTenant, TenantContext } from '@/lib/tenant'
import { OrgClusters } from '@/components/clusters/org-clusters'
import type { ListedCluster, Me } from '@/types/api'

vi.mock('@/lib/api/clusters', () => ({
  fetchOrgClusters: vi.fn(), registerCluster: vi.fn(), removeCluster: vi.fn(), setClusterEnabled: vi.fn(), setClusterOrder: vi.fn(), updateCluster: vi.fn(),
}))

const at = '2026-01-01T00:00:00Z'
const me: Me = {
  kind: 'user',
  user_id: 'u1',
  name: 'Ada',
  orgs: [{ id: 'org_a', name: 'Ada', role: 'owner', created_at: at, actions: ['edit', 'create_api_key'] }],
  projects: [{ id: 'prj_1', org_id: 'org_a', name: 'default', created_at: at }],
}
const member: Me = { ...me, orgs: [{ ...me.orgs[0], role: 'member', actions: ['create_api_key'] }] }

const owned = (name: string, over: Partial<ListedCluster> = {}): ListedCluster =>
  ({ name, org_id: 'org_a', enabled: true, created_at: at, owned: true, connected: false, autoscaler_enabled: false, actions: [], ...over })
const platform: ListedCluster = { name: 'platform-gpu', enabled: true, created_at: at, owned: false, connected: true, autoscaler_enabled: false, place: 4, actions: [] }

// What the API lets the one reading the list do to c: an owner turns any on and off and changes
// the org's own, and a member does nothing.
let viewer: Me
function withActions(c: ListedCluster): ListedCluster {
  const edits = viewer.orgs[0].actions.includes('edit')
  const own: ListedCluster['actions'] = c.owned ? ['edit', 'remove'] : []
  return { ...c, actions: edits ? ['enable', ...own] : [] }
}

// In the order the API lists them: the org's own as it ordered them, then the platform's.
let clusters: ListedCluster[]

function show(as: Me = me) {
  viewer = as
  const resolved = resolveTenant(as, '')
  if (resolved.state !== 'selected') throw new Error('no tenant')
  return render(
    <SWRConfig value={{ provider: () => new Map() }}>
      <TenantContext.Provider value={resolved.tenant}><OrgClusters /></TenantContext.Provider>
    </SWRConfig>,
  )
}

const rowOf = async (name: string) => (await screen.findByRole('cell', { name })).closest('tr')!
const names = () => screen.getAllByRole('row').map(r => r.getAttribute('data-cluster')).filter(Boolean)

beforeEach(() => {
  vi.resetAllMocks()
  clusters = [owned('lab', { connected: true, scale_up_timeout_seconds: 600, place: 1 }), owned('new', { place: 2 }), owned('spare', { place: 3 }), platform]
  vi.mocked(fetchOrgClusters).mockImplementation(async () => clusters.map(withActions))
  vi.mocked(setClusterOrder).mockImplementation(async (_orgID, order) => {
    clusters = [...order.map((n, i) => ({ ...clusters.find(c => c.name === n)!, place: i + 1 })), platform]
    return clusters
  })
  vi.mocked(updateCluster).mockImplementation(async (_orgID, name, change) => {
    clusters = clusters.map(c => (c.name === name ? Object.fromEntries(Object.entries({ ...c, ...change }).filter(([, v]) => v !== 0)) as unknown as ListedCluster : c))
  })
  vi.mocked(removeCluster).mockImplementation(async (_orgID, name) => { clusters = clusters.filter(c => c.name !== name) })
  vi.mocked(setClusterEnabled).mockImplementation(async (_orgID, name, enabled) => {
    clusters = clusters.map(c => (c.name === name ? { ...c, enabled, place: enabled ? 4 : undefined } : c))
  })
  vi.mocked(registerCluster).mockImplementation(async (_orgID, name) => {
    if (!clusters.some(c => c.name === name)) clusters = [owned(name), ...clusters]
    return {
      name, runtime_token: 'hls_cnVudGltZQ.c2ln', attach_commands: { kubernetes: `sh -c 'helm' sh '${name}'`, bare_metal: `sudo sh -c 'install' sh '${name}'` },
      api_url: 'https://api.example.com', metrics_url: 'https://metrics.example.com', registry: 'registry.example.com/hl',
    }
  })
})

describe('the org\'s clusters', () => {
  it('are listed with their status, the platform\'s last and not the org\'s to change, and without what a runtime reports of itself', async () => {
    show()
    expect(within(await rowOf('lab')).getByText('Connected')).toBeTruthy()
    expect(within(await rowOf('new')).getByText('Disconnected')).toBeTruthy()
    const shared = await rowOf('platform-gpu')
    expect(within(shared).getByText('Platform')).toBeTruthy()
    expect(screen.getByRole('columnheader', { name: 'Platform clusters' })).toBeTruthy()
    expect(within(shared).getByText('4')).toBeTruthy()
    expect(within(shared).queryByRole('button')).toBeNull()
    expect(names()).toEqual(['lab', 'new', 'spare', 'platform-gpu'])
  })

  it('say of one that is disconnected when it was last heard from, its agent\'s version against the platform\'s, and its last refusal', async () => {
    clusters = [
      owned('lab', { connected: true, agent_version: 'abc1234' }),
      owned('tt-box', { last_contact_at: '2026-01-01T00:00:00Z', agent_version: 'old1234', version_skew: true, last_error: 'cluster tt-box is bound to another cluster_id', last_error_at: '2026-01-02T00:00:00Z' }),
      owned('never'),
      platform,
    ]
    show()
    const lab = within(await rowOf('lab'))
    expect(lab.getByText('abc1234')).toBeTruthy()
    expect(lab.queryByText('Not the platform\'s version')).toBeNull()
    expect(lab.queryByText(/Last contact/)).toBeNull()
    const box = within(await rowOf('tt-box'))
    expect(box.getByText(/Last contact/)).toBeTruthy()
    expect(box.getByText('old1234')).toBeTruthy()
    expect(box.getByText('Not the platform\'s version')).toBeTruthy()
    expect(box.getByText(/Last refused .*: cluster tt-box is bound to another cluster_id/)).toBeTruthy()
    expect(within(await rowOf('never')).getByText('Not heard from in 30 days · skipped until it reconnects')).toBeTruthy()
  })

  it('are only shown to a member', async () => {
    clusters = clusters.map(c => (c.name === 'platform-gpu' ? { ...c, enabled: false, place: undefined } : c))
    show(member)
    const shared = await rowOf('platform-gpu')
    expect(within(shared).getByRole('switch')).toHaveProperty('disabled', true)
    expect(within(shared).getByText('No new jobs')).toBeTruthy()
    expect(within(shared).getByText('Off')).toBeTruthy()
    expect(screen.queryByRole('button')).toBeNull()
    expect(screen.queryByLabelText('Cluster name')).toBeNull()
  })
})

describe('a cluster the platform rents machines on', () => {
  it('reads by its vendor, with its own name under it', async () => {
    clusters = [{ ...platform, name: 'hypothesisloop-vast', provider: 'vast' }, { ...platform, name: 'hypothesisloop-runpod', provider: 'runpod' }]
    const { container } = show()
    expect(await screen.findByText('Vast.ai')).toBeTruthy()
    expect(screen.getByText('RunPod')).toBeTruthy()
    const row = container.querySelector('tr[data-cluster="hypothesisloop-runpod"]') as HTMLElement
    expect(within(row).getByText('hypothesisloop-runpod · on demand')).toBeTruthy()
    expect(within(row).getByRole('switch', { name: 'Use RunPod for jobs' })).toBeTruthy()
  })
})

describe('where the org\'s jobs run', () => {
  it('is the places the API answers, tied ones marked, with the first connected place named above the table', async () => {
    clusters = [owned('lab', { place: 1 }), owned('spare', { place: 1, connected: true }), owned('old', { enabled: false }), { ...platform, place: 2 }]
    show()
    expect(await screen.findByText(/Next job goes to/)).toBeTruthy()
    expect(screen.getByText(/Next job goes to/).querySelector('code')!.textContent).toBe('spare')
    for (const name of ['lab', 'spare']) {
      const row = within(await rowOf(name))
      expect(row.getByText('1')).toBeTruthy()
      expect(row.getByText('tied, most room first')).toBeTruthy()
    }
    expect(within(await rowOf('lab')).getByText(/skipped until it reconnects/)).toBeTruthy()
    const old = within(await rowOf('old'))
    expect(old.getByText('Off')).toBeTruthy()
    expect(old.queryByText(/skipped until it reconnects/)).toBeNull()
    expect(within(await rowOf('platform-gpu')).queryByText('tied, most room first')).toBeNull()
  })

  it('is the platform\'s compute alone for an org with no cluster of its own, which is told where its own would go', async () => {
    clusters = [{ ...platform, place: 1 }, { ...platform, name: 'platform-tt', place: 1, connected: false }]
    show()
    expect(await screen.findByRole('columnheader', { name: 'Your clusters' })).toBeTruthy()
    expect(screen.getByText('None')).toBeTruthy()
    expect(screen.getByText(/Next job goes to/).querySelector('code')!.textContent).toBe('platform-gpu')
    expect(names()).toEqual(['platform-gpu', 'platform-tt'])
    expect(within(await rowOf('platform-tt')).getByText('Platform')).toBeTruthy()
  })

  it('says jobs wait when no cluster in use is connected, or every one is off', async () => {
    clusters = [owned('lab', { place: 1 }), { ...platform, connected: false, place: 2 }]
    const { unmount } = show()
    expect(await screen.findByText('No cluster in use is connected. Jobs wait in the queue.')).toBeTruthy()
    unmount()
    clusters = [owned('lab', { enabled: false }), { ...platform, enabled: false, place: undefined }]
    show()
    expect(await screen.findByText('All clusters are off. Jobs wait in the queue.')).toBeTruthy()
  })
})

describe('a cluster\'s scheduler settings', () => {
  const advanced = async (name: string) => {
    await userEvent.click(within(await rowOf(name)).getByRole('button', { name: 'Advanced' }))
    return within(screen.getByRole('cell', { name: `Advanced settings of ${name}` }))
  }

  it('are saved as the fields that changed, an emptied one as 0, and a refusal stays beside them', async () => {
    clusters = clusters.map(c => (c.name === 'lab' ? { ...c, autoscaler_enabled: true } : c))
    show()
    const lab = await advanced('lab')
    await userEvent.type(lab.getByLabelText('speculative cap'), 'many')
    await userEvent.click(lab.getByRole('button', { name: 'Save' }))
    expect(lab.getByText('speculative cap must be a whole number above 0, or empty for the default')).toBeTruthy()
    expect(updateCluster).not.toHaveBeenCalled()

    await userEvent.clear(lab.getByLabelText('speculative cap'))
    await userEvent.clear(lab.getByLabelText('scale-up timeout'))
    await userEvent.type(lab.getByLabelText('stuck-pending timeout'), '900')
    await userEvent.click(lab.getByRole('button', { name: 'Save' }))
    expect(updateCluster).toHaveBeenCalledExactlyOnceWith('org_a', 'lab', { scale_up_timeout_seconds: 0, stuck_pending_timeout_seconds: 900 })
    expect(await lab.findByText('stuck-pending timeout: 900s')).toBeTruthy()

    vi.mocked(updateCluster).mockRejectedValue(new Error('scale_up_timeout_seconds must be under 1800'))
    await userEvent.type(lab.getByLabelText('scale-up timeout'), '5000')
    await userEvent.click(lab.getByRole('button', { name: 'Save' }))
    expect(await lab.findByText('scale_up_timeout_seconds must be under 1800')).toBeTruthy()
  })

  it('share their disclosure with removing the cluster', async () => {
    show()
    const lab = await advanced('lab')
    expect(lab.queryByRole('switch')).toBeNull()
    await userEvent.click(lab.getByRole('button', { name: 'Remove cluster' }))
    await userEvent.click(within(await screen.findByRole('dialog')).getByRole('button', { name: 'Remove cluster' }))
    await waitFor(() => expect(removeCluster).toHaveBeenCalledExactlyOnceWith('org_a', 'lab'))
    await waitFor(() => expect(names()).toEqual(['new', 'spare', 'platform-gpu']))
  })
})

describe('turning a cluster off for the org\'s jobs', () => {
  it('is an owner\'s on every row, the platform\'s cluster included, and says what it means', async () => {
    show()
    const row = await rowOf('platform-gpu')
    const use = within(row).getByRole('switch', { name: 'Use platform-gpu for jobs' })
    expect(use.getAttribute('aria-checked')).toBe('true')
    expect(within(await rowOf('lab')).getByRole('switch', { name: 'Use lab for jobs' })).toBeTruthy()

    await userEvent.click(use)
    expect(setClusterEnabled).toHaveBeenCalledExactlyOnceWith('org_a', 'platform-gpu', false)
    expect(await within(row).findByText('No new jobs')).toBeTruthy()
    expect(within(row).getByRole('switch').getAttribute('aria-checked')).toBe('false')

    vi.mocked(setClusterEnabled).mockRejectedValue(new Error('forbidden: only an owner of the org may change it'))
    await userEvent.click(within(row).getByRole('switch'))
    expect(await screen.findByText('forbidden: only an owner of the org may change it')).toBeTruthy()
    expect(within(row).getByRole('switch').getAttribute('aria-checked')).toBe('false')
  })
})

describe('the order of the org\'s own clusters', () => {
  it('is changed by an owner with the buttons or by dragging, as one order the API writes whole', async () => {
    show()
    await screen.findByRole('button', { name: 'Move spare up' })
    expect(screen.getByRole('button', { name: 'Move lab up' })).toHaveProperty('disabled', true)
    expect(screen.queryByRole('button', { name: /Move platform-gpu/ })).toBeNull()

    await userEvent.click(screen.getByRole('button', { name: 'Move spare up' }))
    await waitFor(() => expect(setClusterOrder).toHaveBeenCalledExactlyOnceWith('org_a', ['lab', 'spare', 'new']))
    await waitFor(() => expect(names()).toEqual(['lab', 'spare', 'new', 'platform-gpu']))
    fireEvent.dragStart(await rowOf('new'))
    fireEvent.dragOver(await rowOf('lab'))
    fireEvent.drop(await rowOf('lab'))
    await waitFor(() => expect(setClusterOrder).toHaveBeenLastCalledWith('org_a', ['new', 'lab', 'spare']))
    await waitFor(() => expect(names()).toEqual(['new', 'lab', 'spare', 'platform-gpu']))
  })

  it('goes back to what it was when the API refuses, with the refusal', async () => {
    vi.mocked(setClusterOrder).mockRejectedValue(new Error('forbidden: only an owner of the org may change it'))
    show()
    await userEvent.click(await screen.findByRole('button', { name: 'Move new up' }))
    expect(await screen.findByText('forbidden: only an owner of the org may change it')).toBeTruthy()
    await waitFor(() => expect(names()).toEqual(['lab', 'new', 'spare', 'platform-gpu']))
  })
})

describe('adding a cluster', () => {
  it('shows the command the API answered for the kind chosen beside it, with the token it asks for, and lists the cluster', async () => {
    show()
    await userEvent.type(await screen.findByLabelText('Cluster name'), 'edge')
    await userEvent.click(screen.getByRole('button', { name: 'Add cluster' }))
    expect(registerCluster).toHaveBeenCalledExactlyOnceWith('org_a', 'edge')
    expect((await screen.findByTestId('secret')).textContent).toBe(`sh -c 'helm' sh 'edge'`)
    // The machine has no hl to read the token with, so it is shown beside the command that asks for it.
    expect(screen.getByTestId('runtime-token').textContent).toBe('hls_cnVudGltZQ.c2ln')
    expect(await screen.findByRole('cell', { name: 'edge' })).toBeTruthy()
    expect((screen.getByLabelText('Cluster name') as HTMLInputElement).value).toBe('')
    // Beside the command: what it needs, what it installs where, and how it is checked and removed.
    const before = within(screen.getByRole('region', { name: 'Before you run it' }))
    expect(before.getByText('https://api.example.com')).toBeTruthy()
    expect(before.getByText('registry.example.com/hl')).toBeTruthy()
    expect(within(screen.getByRole('region', { name: 'What it installs' })).getByText('hypothesisloop-runtime-token')).toBeTruthy()
    expect(screen.getByText('--dry-run=server')).toBeTruthy()
    expect(screen.getByText('helm uninstall hypothesisloop-cluster-agent -n hypothesisloop')).toBeTruthy()

    const kinds = within(screen.getByRole('group', { name: 'Kind of cluster' }))
    expect(kinds.getByRole('button', { name: 'Kubernetes cluster' }).getAttribute('aria-pressed')).toBe('true')
    await userEvent.click(kinds.getByRole('button', { name: 'Bare-metal node' }))
    expect(screen.getByTestId('secret').textContent).toBe(`sudo sh -c 'install' sh 'edge'`)
    expect(screen.getByText(/sudo bare-agent detach/)).toBeTruthy()
    expect(screen.getByText('/etc/hypothesisloop/bare-agent.env')).toBeTruthy()
    expect(screen.getByText('--dry-run')).toBeTruthy()
    expect(registerCluster).toHaveBeenCalledTimes(1)

    await userEvent.click(screen.getByRole('button', { name: 'Hide' }))
    expect(screen.queryByTestId('secret')).toBeNull()
    expect(document.body.textContent).not.toContain('hls_')

    // Registering an owned name answers the same, so a row brings its command back.
    await userEvent.click(within(await rowOf('lab')).getByRole('button', { name: 'Show start command' }))
    expect(registerCluster).toHaveBeenLastCalledWith('org_a', 'lab')
    expect((await screen.findByTestId('secret')).textContent).toBe(`sh -c 'helm' sh 'lab'`)
  })

  it('keeps a name the API refuses in the field, with the refusal beside it', async () => {
    vi.mocked(registerCluster).mockRejectedValueOnce(new Error('cluster name "Lab K8s" must be lowercase letters, digits and dashes'))
    show()
    const field = await screen.findByLabelText('Cluster name') as HTMLInputElement
    await userEvent.type(field, 'Lab K8s')
    await userEvent.click(screen.getByRole('button', { name: 'Add cluster' }))
    expect(await screen.findByText(/must be lowercase letters, digits and dashes/)).toBeTruthy()
    expect(field.value).toBe('Lab K8s')
  })
})

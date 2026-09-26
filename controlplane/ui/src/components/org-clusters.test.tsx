import { render, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { SWRConfig } from 'swr'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { fetchClusterSettings, fetchOrgClusters, putClusterSettings, registerCluster, setClusterEnabled } from '@/lib/api'
import { resolveTenant, TenantContext } from '@/lib/tenant'
import { OrgClusters } from '@/components/org-clusters'
import type { ClusterSettings, Me, OrgCluster } from '@/types'

vi.mock('@/lib/api', () => ({
  fetchClusterSettings: vi.fn(), fetchOrgClusters: vi.fn(), putClusterSettings: vi.fn(), registerCluster: vi.fn(),
  setClusterEnabled: vi.fn(),
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
    { name: 'lab', org_id: 'org_a', shared: false, enabled: true, cluster_id: 'cid-1', created_at: at, owned: true, connected: true },
    { name: 'new', org_id: 'org_a', shared: false, enabled: true, created_at: at, owned: true, connected: false },
  ]
  settings = { cluster_id: 'cid-1', scale_up_timeout_seconds: 600, max_speculative_accelerators: null, stuck_pending_timeout_seconds: null }
  vi.mocked(fetchOrgClusters).mockImplementation(async () => [...clusters])
  vi.mocked(fetchClusterSettings).mockImplementation(async () => ({ ...settings }))
  vi.mocked(putClusterSettings).mockImplementation(async (id, next) => (settings = { cluster_id: id, ...next }))
  vi.mocked(setClusterEnabled).mockReset()
  vi.mocked(setClusterEnabled).mockImplementation(async (_orgID, name, enabled) => {
    clusters = clusters.map(c => (c.name === name ? { ...c, enabled } : c))
  })
  vi.mocked(registerCluster).mockImplementation(async (orgID, name) => {
    if (!clusters.some(c => c.name === name)) {
      clusters = [...clusters, { name, org_id: orgID, shared: false, enabled: true, created_at: at, owned: true, connected: false }]
    }
    return { name, runtime_token: 'hls_cnVudGltZQ.c2ln', api_url: 'http://api.cluster.test', metrics_url: 'http://metrics.cluster.test', registry: 'registry.cluster.test/images', image_tag: 'abc1234' }
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
    clusters = [...clusters, { name: 'platform-gpu', shared: true, enabled: true, created_at: at, owned: false, connected: true }]
  })

  it('is listed with its status, as the platform\'s, with settings that are not the org\'s to change', async () => {
    show()
    const row = (await screen.findByRole('cell', { name: /^platform-gpu/ })).closest('tr')!
    expect(within(row).getByText('Connected')).toBeTruthy()
    expect(within(row).getByText('Platform')).toBeTruthy()
    expect(within(row).getByText('shared with every org')).toBeTruthy()
    expect(within(row).getByText('set by the platform')).toBeTruthy()
    expect(within(row).queryByRole('button')).toBeNull()
    expect(fetchClusterSettings).toHaveBeenCalledExactlyOnceWith('cid-1')
  })
})

describe('turning a cluster off for the org\'s jobs', () => {
  beforeEach(() => {
    clusters = [...clusters, { name: 'platform-gpu', shared: true, enabled: true, created_at: at, owned: false, connected: true }]
  })

  const rowOf = async (name: RegExp) => (await screen.findByRole('cell', { name })).closest('tr')!
  const member: Me = { ...me, orgs: [{ ...me.orgs[0], role: 'member' }] }

  it('is an owner\'s on every row, the platform\'s shared cluster included, and says what it means', async () => {
    show()
    const row = await rowOf(/^platform-gpu/)
    const use = within(row).getByRole('switch', { name: 'Use platform-gpu for the jobs of Ada' })
    expect(use.getAttribute('aria-checked')).toBe('true')
    expect(within(row).getByText('Used')).toBeTruthy()
    expect(within(await rowOf(/^lab/)).getByRole('switch', { name: 'Use lab for the jobs of Ada' })).toBeTruthy()

    await userEvent.click(use)
    expect(setClusterEnabled).toHaveBeenCalledExactlyOnceWith('org_a', 'platform-gpu', false)
    expect(await within(row).findByText('Not used')).toBeTruthy()
    expect(within(row).getByText('Turned off: no job of Ada is placed here. Jobs already running finish.')).toBeTruthy()
    expect(within(row).getByRole('switch').getAttribute('aria-checked')).toBe('false')
    expect(within(await rowOf(/^lab/)).getByText('Used')).toBeTruthy()

    await userEvent.click(within(row).getByRole('switch'))
    expect(setClusterEnabled).toHaveBeenLastCalledWith('org_a', 'platform-gpu', true)
    expect(await within(row).findByText('Used')).toBeTruthy()
    expect(within(row).queryByText(/Jobs already running finish/)).toBeNull()
  })

  it('shows the API\'s refusal and leaves the cluster as it was', async () => {
    vi.mocked(setClusterEnabled).mockRejectedValue(new Error('forbidden: only an owner of the org may change it'))
    show()
    const row = await rowOf(/^platform-gpu/)
    await userEvent.click(within(row).getByRole('switch'))
    expect(await screen.findByText('forbidden: only an owner of the org may change it')).toBeTruthy()
    expect(within(row).getByRole('switch').getAttribute('aria-checked')).toBe('true')
    expect(within(row).getByText('Used')).toBeTruthy()
  })

  it('is shown to a member as it stands, with nothing to switch', async () => {
    clusters = clusters.map(c => (c.name === 'platform-gpu' ? { ...c, enabled: false } : c))
    show({ as: member })
    const row = await rowOf(/^platform-gpu/)
    expect(within(row).getByText('Not used')).toBeTruthy()
    expect(within(row).getByText(/no job of Ada is placed here/)).toBeTruthy()
    expect(within(await rowOf(/^lab/)).getByText('Used')).toBeTruthy()
    expect(screen.queryByRole('switch')).toBeNull()
  })

  it('is shown and not offered where clusters are only shown', async () => {
    clusters = clusters.map(c => (c.name === 'lab' ? { ...c, enabled: false } : c))
    show({ manage: false })
    expect(within(await rowOf(/^lab/)).getByText('Not used')).toBeTruthy()
    expect(screen.queryByRole('switch')).toBeNull()
  })
})

describe('a cluster\'s start command', () => {
  it('is shown again on request: registering an owned name answers the same', async () => {
    show()
    const lab = (await screen.findByRole('cell', { name: /^lab/ })).closest('tr')!
    await userEvent.click(within(lab).getByRole('button', { name: 'Show start command' }))
    expect(registerCluster).toHaveBeenCalledExactlyOnceWith('org_a', 'lab')
    expect((await screen.findByTestId('secret')).textContent).toContain(`hl cluster token 'lab' --org 'org_a' | helm upgrade --install hypothesisloop-cluster-agent `)
    expect(screen.getByText(/brings it back/)).toBeTruthy()
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
      `hl cluster token 'edge' --org 'org_a' | helm upgrade --install hypothesisloop-cluster-agent 'oci://registry.cluster.test/images/charts/hypothesisloop-cluster-agent' --version '0.1.0+abc1234' --namespace hypothesisloop --create-namespace ` +
      `--set-string 'clusterName=edge' --set-string 'apiURL=http://api.cluster.test' --set-string 'metricsURL=http://metrics.cluster.test' ` +
      `--set-string 'images.registry=registry.cluster.test/images' --set-string 'images.tag=abc1234' --set-file runtimeToken=/dev/stdin`,
    )
    expect(document.body.textContent).not.toContain('hls_')
    expect(await screen.findByRole('cell', { name: 'edge' })).toBeTruthy()
    await userEvent.click(screen.getByRole('button', { name: 'Hide' }))
    expect(screen.queryByTestId('secret')).toBeNull()
  })

  it('asks which kind first, and shows a bare-metal node the command that runs its agent', async () => {
    show()
    const kinds = within(await screen.findByRole('group', { name: 'Kind of cluster' }))
    expect(kinds.getByRole('button', { name: 'Kubernetes cluster' }).getAttribute('aria-pressed')).toBe('true')
    await userEvent.click(kinds.getByRole('button', { name: 'Bare-metal node' }))
    expect(screen.getByText(/starts jobs as containers with docker or podman/)).toBeTruthy()
    await userEvent.type(screen.getByLabelText('Cluster name'), 'gpu-box-1')
    await userEvent.click(screen.getByRole('button', { name: 'Add node' }))
    expect(registerCluster).toHaveBeenCalledExactlyOnceWith('org_a', 'gpu-box-1')
    expect((await screen.findByTestId('secret')).textContent).toBe(
      `sudo sh -c 'engine="$(command -v docker || command -v podman)" || { echo "docker or podman is needed on this node" >&2; exit 1; }; ` +
      'staged="$("$engine" create "$3")" && "$engine" cp "$staged:/usr/local/bin/bare-agent" /usr/local/bin/bare-agent.new && "$engine" rm "$staged" >/dev/null && ' +
      `exec /usr/local/bin/bare-agent.new install --cluster "$1" --api-url "$2"' sh 'gpu-box-1' 'http://api.cluster.test' 'registry.cluster.test/images/hypothesisloop-bare-agent:abc1234'`,
    )
    expect(screen.getByText(/Run it on the node/)).toBeTruthy()
    expect(screen.getByText(/sudo bare-agent detach/)).toBeTruthy()
    // The node has no hl to read the token with, so it is shown beside the command that asks for it.
    expect(screen.getByTestId('runtime-token').textContent).toBe('hls_cnVudGltZQ.c2ln')
    await userEvent.click(screen.getByRole('button', { name: 'Kubernetes cluster' }))
    expect(screen.queryByTestId('runtime-token')).toBeNull()
    expect(document.body.textContent).not.toContain('hls_')
  })

  it('switches the command on show when the other kind is chosen, without asking the API again', async () => {
    show()
    await userEvent.type(await screen.findByLabelText('Cluster name'), 'edge')
    await userEvent.click(screen.getByRole('button', { name: 'Add cluster' }))
    expect((await screen.findByTestId('secret')).textContent).toContain('helm upgrade --install')
    await userEvent.click(screen.getByRole('button', { name: 'Bare-metal node' }))
    expect(screen.getByTestId('secret').textContent).toContain('hypothesisloop-bare-agent:')
    expect(screen.getByTestId('secret').textContent).not.toContain('metricsURL')
    expect(registerCluster).toHaveBeenCalledTimes(1)
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

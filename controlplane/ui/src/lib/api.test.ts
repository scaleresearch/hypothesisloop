import { beforeEach, describe, expect, it, vi } from 'vitest'
import { accessToken, endSession } from '@/lib/auth'
import {
  ApiError, addHumanHypothesisComment, registerCluster, connectGitHubInstallation, disconnectGitHubInstallation, fetchGitHubApp, fetchGitHubInstallations, fetchGitHubRepositories, createAPIKey, createPlatformExperiment, fetchAgentsPage, fetchMe,
  fetchPlatformExperimentAgentToken, fetchPlatformExperimentsPage, fetchResourceCapacity, putClusterSettings, setClusterEnabled,
  registerHumanHypothesis, revokeAPIKey,
  acceptOrgInvite, createOrgInvite, fetchOrgInvites, fetchOrgMembers, removeOrgMember, revokeOrgInvite, setOrgMemberRole,
} from '@/lib/api'

vi.mock('@/lib/auth', () => ({ accessToken: vi.fn(), endSession: vi.fn() }))

const SESSION_OVER = new Error('the session ended')

function answer(status: number, body: unknown, headers: Record<string, string> = {}) {
  return new Response(typeof body === 'string' ? body : JSON.stringify(body), { status, headers })
}

function fetchMock() {
  return vi.mocked(globalThis.fetch)
}

// The one request the helper under test made: its URL, its headers and its JSON body.
function sent() {
  expect(fetchMock()).toHaveBeenCalledTimes(1)
  const [url, init] = fetchMock().mock.calls[0] as [string, RequestInit]
  return {
    url: new URL(url),
    init,
    headers: init.headers as Record<string, string>,
    body: init.body ? JSON.parse(init.body as string) : undefined,
  }
}

beforeEach(() => {
  vi.stubGlobal('fetch', vi.fn())
  vi.mocked(accessToken).mockResolvedValue('token-1')
  vi.mocked(endSession).mockRejectedValue(SESSION_OVER)
})

describe('every request carries the session token', () => {
  it('on a read', async () => {
    fetchMock().mockResolvedValue(answer(200, { kind: 'user', orgs: [], projects: [] }))
    await expect(fetchMe()).resolves.toMatchObject({ kind: 'user' })
    expect(sent().headers.Authorization).toBe('Bearer token-1')
    expect(sent().init.cache).toBe('no-store')
    expect(sent().url.pathname).toBe('/me')
  })

  it('on a paged read, which also names the project and reads the total', async () => {
    fetchMock().mockResolvedValue(answer(200, [{ id: 'pe-1' }], { 'X-Total-Count': '41' }))
    const page = await fetchPlatformExperimentsPage({ project_id: 'prj_1', status: 'open', q: '' })
    expect(page).toEqual({ items: [{ id: 'pe-1' }], total: 41 })
    expect(sent().headers.Authorization).toBe('Bearer token-1')
    expect(sent().url.searchParams.get('project_id')).toBe('prj_1')
    expect(sent().url.searchParams.get('status')).toBe('open')
    expect(sent().url.searchParams.has('q')).toBe(false)
  })

  it('on a write with a body', async () => {
    fetchMock().mockResolvedValue(answer(201, { id: 'pe-1' }))
    await createPlatformExperiment('prj_1', {
      name: 'n', description: '', metrics: [], report_interval_seconds: 60, budget_accelerator_hours: 1, max_agents: 1, starts_at: 'a', ends_at: 'b',
    })
    const { headers, init, body, url } = sent()
    expect(init.method).toBe('POST')
    expect(url.pathname).toBe('/platform-experiments')
    expect(headers.Authorization).toBe('Bearer token-1')
    expect(headers['Content-Type']).toBe('application/json')
    expect(body).toMatchObject({ name: 'n', project_id: 'prj_1' })
  })

  it('on a write without a body', async () => {
    fetchMock().mockResolvedValue(answer(200, { id: 'k1', revoked_at: 'now' }))
    await revokeAPIKey('k1')
    expect(sent().init.method).toBe('DELETE')
    expect(sent().headers.Authorization).toBe('Bearer token-1')
    expect(sent().init.body).toBeUndefined()
  })

  it('asks for the token again on each request, so a refreshed one is used at once', async () => {
    fetchMock().mockImplementation(async () => answer(200, [], { 'X-Total-Count': '0' }))
    await fetchAgentsPage({ project_id: 'prj_1' })
    vi.mocked(accessToken).mockResolvedValue('token-2')
    await fetchAgentsPage({ project_id: 'prj_1' })
    const tokens = fetchMock().mock.calls.map(([, init]) => (init!.headers as Record<string, string>).Authorization)
    expect(tokens).toEqual(['Bearer token-1', 'Bearer token-2'])
  })
})

describe('an answer that is not what was asked for is an error', () => {
  it('for a list without its total', async () => {
    fetchMock().mockResolvedValue(answer(200, [{ id: 'ag-1' }]))
    await expect(fetchAgentsPage({ project_id: 'prj_1' })).rejects.toThrow('/agents?project_id=prj_1 answered a list without X-Total-Count')
  })

  it('for a token request answered with no agent', async () => {
    fetchMock().mockResolvedValue(answer(200, { agents: [] }))
    await expect(fetchPlatformExperimentAgentToken('pe-1', 'ag-1')).rejects.toThrow('asked for the token of agent ag-1 and was answered 0 agents')
  })

  it('for a call made with no API address configured', async () => {
    vi.stubEnv('NEXT_PUBLIC_API_URL', '')
    await expect(fetchMe()).rejects.toThrow('NEXT_PUBLIC_API_URL is not set')
    expect(fetchMock()).not.toHaveBeenCalled()
    vi.unstubAllEnvs()
  })
})

describe('live capacity', () => {
  it('is read for the org named', async () => {
    fetchMock().mockResolvedValue(answer(200, { clusters: [] }))
    await fetchResourceCapacity('org_b')
    expect(sent().url.href).toBe('http://api.test/resource-catalog/capacity?org_id=org_b')
  })
})

describe('a cluster turned off or on for an org\'s jobs', () => {
  it('is one POST to the verb, with the name as a path segment', async () => {
    fetchMock().mockResolvedValue(answer(200, { name: 'lab', enabled: false }))
    await setClusterEnabled('org_b', 'lab', false)
    expect(sent().init.method).toBe('POST')
    expect(sent().url.href).toBe('http://api.test/orgs/org_b/clusters/lab/disable')
    fetchMock().mockClear()
    fetchMock().mockResolvedValue(answer(200, { name: 'lab', enabled: true }))
    await setClusterEnabled('org_b', 'lab', true)
    expect(sent().url.href).toBe('http://api.test/orgs/org_b/clusters/lab/enable')
  })
})

describe('a session that is gone or refused has one outcome', () => {
  it('ends the session when the API answers 401 to a read', async () => {
    fetchMock().mockResolvedValue(answer(401, { error: 'unauthenticated: token has expired' }))
    await expect(fetchMe()).rejects.toBe(SESSION_OVER)
    expect(endSession).toHaveBeenCalledExactlyOnceWith('refused')
  })

  it('ends the session when the API answers 401 to a paged read', async () => {
    fetchMock().mockResolvedValue(answer(401, { error: 'unauthenticated' }))
    await expect(fetchAgentsPage({ project_id: 'prj_1' })).rejects.toBe(SESSION_OVER)
    expect(endSession).toHaveBeenCalledExactlyOnceWith('refused')
  })

  it('ends the session when the API answers 401 to a write', async () => {
    fetchMock().mockResolvedValue(answer(401, { error: 'unauthenticated' }))
    await expect(createAPIKey('org_1', 'laptop')).rejects.toBe(SESSION_OVER)
    expect(endSession).toHaveBeenCalledExactlyOnceWith('refused')
  })

  it('ends the session without calling the API when there is no token', async () => {
    vi.mocked(accessToken).mockResolvedValue(null)
    await expect(fetchMe()).rejects.toBe(SESSION_OVER)
    expect(endSession).toHaveBeenCalledExactlyOnceWith('ended')
    expect(fetchMock()).not.toHaveBeenCalled()
  })

  it('fails the request, and keeps the session, when the identity provider cannot be reached', async () => {
    vi.mocked(accessToken).mockRejectedValue(new Error('network down'))
    await expect(fetchMe()).rejects.toThrow('network down')
    expect(endSession).not.toHaveBeenCalled()
    expect(fetchMock()).not.toHaveBeenCalled()
  })

  it('keeps the session on a 403 and a 404, which are answers about one thing', async () => {
    fetchMock().mockImplementation(async () => answer(403, { error: 'forbidden: only an owner of the org may change it' }))
    await expect(createAPIKey('org_1', 'laptop')).rejects.toThrow('forbidden: only an owner of the org may change it')
    expect(endSession).not.toHaveBeenCalled()
  })
})

describe('a failure carries the API\'s own words', () => {
  it('from the {"error"} envelope', async () => {
    fetchMock().mockResolvedValue(answer(404, { error: 'org not found' }))
    const error = await fetchMe().catch(e => e)
    expect(error).toBeInstanceOf(ApiError)
    expect(error).toMatchObject({ status: 404, message: 'org not found' })
  })

  it('from the {"reason","message"} envelope', async () => {
    fetchMock().mockResolvedValue(answer(409, { reason: 'experiment_started', message: 'experiment_started: experiment is running' }))
    await expect(putClusterSettings('c1', {
      scale_up_timeout_seconds: null, max_speculative_accelerators: null, stuck_pending_timeout_seconds: 60,
    })).rejects.toMatchObject({ status: 409, message: 'experiment_started: experiment is running' })
  })

  it('from a body that is not JSON', async () => {
    fetchMock().mockResolvedValue(answer(502, 'bad gateway'))
    await expect(fetchMe()).rejects.toMatchObject({ status: 502, message: 'bad gateway' })
  })

  it('from an empty body', async () => {
    fetchMock().mockResolvedValue(answer(500, ''))
    await expect(fetchMe()).rejects.toMatchObject({ status: 500, message: 'API error 500' })
  })
})

describe('what a person writes is filed under their session, with no author in the body', () => {
  it('for an idea', async () => {
    fetchMock().mockResolvedValue(answer(201, { id: 'h1', already_existed: false }))
    await registerHumanHypothesis('pe-1', 'an idea', [{ type: 'builds_on', to: 'pe-1' }])
    expect(sent().body).toEqual({
      platform_experiment_id: 'pe-1', text: 'an idea', links: [{ type: 'builds_on', to: 'pe-1' }], distinct_from: [],
    })
  })

  it('for a comment', async () => {
    fetchMock().mockResolvedValue(answer(201, { id: 'c1' }))
    await addHumanHypothesisComment('h1', 'a note')
    expect(sent().body).toEqual({ text: 'a note' })
  })

  it('returns the hypotheses an idea was held back for', async () => {
    fetchMock().mockResolvedValue(answer(409, { similar: [{ id: 'h2', status: 'open', text: 't', reason: 'close_text' }] }))
    await expect(registerHumanHypothesis('pe-1', 'an idea', [])).resolves.toEqual({
      similar: [{ id: 'h2', status: 'open', text: 't', reason: 'close_text' }],
    })
  })
})

describe('registering a cluster', () => {
  it('answers with the cluster, its addresses and its runtime token, and nothing else of the answer', async () => {
    const installedWith = { api_url: 'http://api.cluster.test', metrics_url: 'http://metrics.cluster.test', registry: 'registry.cluster.test/images', image_tag: 'abc1234' }
    fetchMock().mockResolvedValue(answer(200, { name: 'edge', org_id: 'org_a', shared: false, runtime_token: 'hls_runtime', ...installedWith }))
    await expect(registerCluster('org_a', 'edge')).resolves.toEqual({ name: 'edge', runtime_token: 'hls_runtime', ...installedWith })
    expect(sent().body).toEqual({ name: 'edge' })
  })
})

describe('an org\'s GitHub accounts', () => {
  it('are read as a page with its total', async () => {
    fetchMock().mockResolvedValue(answer(200, [{ installation_id: 7, account_login: 'acme' }], { 'X-Total-Count': '1' }))
    await expect(fetchGitHubInstallations('org_a')).resolves.toEqual({ items: [{ installation_id: 7, account_login: 'acme' }], total: 1 })
    expect(sent().url.pathname).toBe('/orgs/org_a/github-installations')
    expect(sent().url.searchParams.get('limit')).toBe('200')
  })

  it('are connected with the code GitHub returned and where it returned it, naming no installation', async () => {
    fetchMock().mockResolvedValue(answer(201, { org_id: 'org_a', installation_id: 7, account_login: 'acme' }))
    await connectGitHubInstallation('org_a', { code: 'code-1', redirectURI: 'http://ui.test/settings/repository' })
    expect(sent().init.method).toBe('POST')
    expect(sent().url.pathname).toBe('/orgs/org_a/github-installations')
    expect(sent().body).toEqual({ code: 'code-1', redirect_uri: 'http://ui.test/settings/repository' })
  })

  it('are connected by installation once the person chose one', async () => {
    fetchMock().mockResolvedValue(answer(201, { org_id: 'org_a', installation_id: 9, account_login: 'lab' }))
    await connectGitHubInstallation('org_a', { code: 'c', redirectURI: 'u', installationID: 9 })
    expect(sent().body).toEqual({ code: 'c', redirect_uri: 'u', installation_id: 9 })
  })

  it('carry the installations to choose from when the person reaches several', async () => {
    const installations = [{ installation_id: 7, account_login: 'acme' }, { installation_id: 9, account_login: 'lab' }]
    fetchMock().mockResolvedValue(answer(409, { reason: 'installation_choice_required', message: 'name one', installations }))
    const refusal = await connectGitHubInstallation('org_a', { code: 'c', redirectURI: 'u' }).catch(e => e)
    expect(refusal).toBeInstanceOf(ApiError)
    expect(refusal).toMatchObject({ status: 409, reason: 'installation_choice_required', message: 'name one' })
    expect(refusal.body.installations).toEqual(installations)
  })

  it('carry the API\'s reason for a refusal, as when the App is installed nowhere the person reaches', async () => {
    fetchMock().mockResolvedValue(answer(422, { reason: 'app_not_installed', message: 'you reach no installation' }))
    await expect(connectGitHubInstallation('org_a', { code: 'c', redirectURI: 'u' })).rejects.toMatchObject({
      status: 422, reason: 'app_not_installed', message: 'you reach no installation',
    })
  })

  it('are disconnected by a call that answers with no body', async () => {
    fetchMock().mockResolvedValue(new Response(null, { status: 204 }))
    await expect(disconnectGitHubInstallation('org_a', 7)).resolves.toBeUndefined()
    expect(sent().init.method).toBe('DELETE')
    expect(sent().url.pathname).toBe('/orgs/org_a/github-installations/7')
  })

  it('list their repositories by a search the server runs', async () => {
    fetchMock().mockResolvedValue(answer(200, [{ repository: 'acme/widgets', installation_id: 7, private: true }], { 'X-Total-Count': '12' }))
    await expect(fetchGitHubRepositories('org_a', { q: 'wid', limit: 50 })).resolves.toEqual({
      items: [{ repository: 'acme/widgets', installation_id: 7, private: true }], total: 12,
    })
    expect(sent().url.pathname).toBe('/orgs/org_a/github-repositories')
    expect(sent().url.search).toBe('?q=wid&limit=50')
  })

  it('cannot be connected on a deployment without a GitHub App, which answers 503', async () => {
    fetchMock().mockResolvedValue(answer(503, { reason: 'github_not_configured', message: 'this deployment has no GitHub App configured' }))
    await expect(fetchGitHubApp()).rejects.toMatchObject({ status: 503, reason: 'github_not_configured' })
  })
})

describe('org members and invites', () => {
  it('reads members and pending invites as a page with its total', async () => {
    fetchMock().mockImplementation(async () => answer(200, [{ user_id: 'u1' }], { 'X-Total-Count': '3' }))
    await expect(fetchOrgMembers('org_a')).resolves.toEqual({ items: [{ user_id: 'u1' }], total: 3 })
    expect(sent().url.pathname).toBe('/orgs/org_a/members')
    expect(sent().url.searchParams.get('limit')).toBe('200')
    fetchMock().mockClear()
    await fetchOrgInvites('org_a')
    expect(sent().url.pathname).toBe('/orgs/org_a/invites')
  })

  it('changes a role with a PATCH naming the member', async () => {
    fetchMock().mockResolvedValue(answer(200, { user_id: 'u2', role: 'owner' }))
    await setOrgMemberRole('org_a', 'u2', 'owner')
    expect(sent().init.method).toBe('PATCH')
    expect(sent().url.pathname).toBe('/orgs/org_a/members/u2')
    expect(sent().body).toEqual({ role: 'owner' })
  })

  it('takes an answer without a body for a removal and a revoke', async () => {
    fetchMock().mockImplementation(async () => new Response(null, { status: 204 }))
    await expect(removeOrgMember('org_a', 'u2')).resolves.toBeUndefined()
    expect(sent().init.method).toBe('DELETE')
    expect(sent().url.pathname).toBe('/orgs/org_a/members/u2')
    fetchMock().mockClear()
    await expect(revokeOrgInvite('inv_1')).resolves.toBeUndefined()
    expect(sent().url.pathname).toBe('/invites/inv_1')
  })

  it('refuses the last owner with the API\'s reason and words', async () => {
    fetchMock().mockResolvedValue(answer(409, { reason: 'last_owner', message: 'an org keeps at least one owner: make someone else an owner first' }))
    await expect(removeOrgMember('org_a', 'u1')).rejects.toMatchObject({
      status: 409, reason: 'last_owner', message: 'an org keeps at least one owner: make someone else an owner first',
    })
  })

  it('creates an invite with a role and returns its token', async () => {
    fetchMock().mockResolvedValue(answer(201, { id: 'inv_1', role: 'member', token: 'hli_abc' }))
    await expect(createOrgInvite('org_a', 'member')).resolves.toMatchObject({ token: 'hli_abc' })
    expect(sent().url.pathname).toBe('/orgs/org_a/invites')
    expect(sent().body).toEqual({ role: 'member' })
  })

  it('accepts an invite with its token in the body, never in the address', async () => {
    fetchMock().mockResolvedValue(answer(201, { org_id: 'org_a', user_id: 'u2', role: 'member' }))
    await acceptOrgInvite('hli_abc')
    expect(sent().init.method).toBe('POST')
    expect(sent().url.pathname).toBe('/invites/accept')
    expect(sent().url.href).not.toContain('hli_abc')
    expect(sent().body).toEqual({ token: 'hli_abc' })
  })

  it('gives a refused invite its reason', async () => {
    fetchMock().mockResolvedValue(answer(409, { reason: 'invite_expired', message: 'the invite has expired: ask an owner of the org for a new one' }))
    const refusal = await acceptOrgInvite('hli_abc').catch((e: unknown) => e)
    expect(refusal).toBeInstanceOf(ApiError)
    expect(refusal).toMatchObject({ status: 409, reason: 'invite_expired' })
  })
})

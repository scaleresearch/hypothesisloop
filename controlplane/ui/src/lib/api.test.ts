import { beforeEach, describe, expect, it, vi } from 'vitest'
import { accessToken, endSession } from '@/lib/auth'
import {
  ApiError, addHumanHypothesisComment, attachProjectRepo, detachProjectRepo, fetchGitHubApp, fetchProjectRepo, createAPIKey, createPlatformExperiment, fetchAgentsPage, fetchMe,
  fetchPlatformExperimentAgentToken, fetchPlatformExperimentsPage, fetchResourceCapacity, putClusterSettings,
  registerHumanHypothesis, revokeAPIKey,
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
      name: 'n', budget_accelerator_hours: 1, max_agents: 1, starts_at: 'a', ends_at: 'b',
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

describe('a project\'s repository', () => {
  it('is null for a project that has none, which the API answers 404', async () => {
    fetchMock().mockResolvedValue(answer(404, { message: 'no repository is attached to this project' }))
    await expect(fetchProjectRepo('prj_1')).resolves.toBeNull()
    expect(sent().url.pathname).toBe('/projects/prj_1/repo')
  })

  it('is a failure when the read fails any other way', async () => {
    fetchMock().mockResolvedValue(answer(500, { message: 'internal error' }))
    await expect(fetchProjectRepo('prj_1')).rejects.toThrow('internal error')
  })

  it('is attached with the installation, the name and the code GitHub returned', async () => {
    fetchMock().mockResolvedValue(answer(200, { project_id: 'prj_1', repo: 'acme/widgets' }))
    await attachProjectRepo('prj_1', 4242, 'acme/widgets', 'code-1')
    expect(sent().init.method).toBe('POST')
    expect(sent().url.pathname).toBe('/projects/prj_1/repo')
    expect(sent().body).toEqual({ installation_id: 4242, repo: 'acme/widgets', code: 'code-1' })
  })

  it('is detached by a call that answers with no body', async () => {
    fetchMock().mockResolvedValue(new Response(null, { status: 204 }))
    await expect(detachProjectRepo('prj_1')).resolves.toBeUndefined()
    expect(sent().init.method).toBe('DELETE')
  })

  it('cannot be attached on a deployment without a GitHub App, which answers 503', async () => {
    fetchMock().mockResolvedValue(answer(503, { message: 'this deployment has no GitHub App configured' }))
    await expect(fetchGitHubApp()).rejects.toMatchObject({ status: 503 })
  })
})

import { render, screen, waitFor, within } from '@testing-library/react'
import { choose } from '@/test-select'
import userEvent from '@testing-library/user-event'
import { SWRConfig } from 'swr'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { createAPIKey, fetchAPIKeys, revokeAPIKey } from '@/lib/api'
import { resolveTenant, TenantContext } from '@/lib/tenant'
import { APIKeys } from '@/components/api-keys'
import type { APIKey, Me } from '@/types'

vi.mock('@/lib/api', () => ({ createAPIKey: vi.fn(), fetchAPIKeys: vi.fn(), revokeAPIKey: vi.fn() }))

const at = '2026-01-01T00:00:00Z'
const TOKEN = 'hlk_k2_the-secret-shown-once'
const me: Me = {
  kind: 'user',
  user_id: 'u1',
  name: 'Ada',
  orgs: [{ id: 'org_a', name: 'Ada', role: 'owner', created_at: at }],
  projects: [
    { id: 'prj_1', org_id: 'org_a', name: 'default', created_at: at },
    { id: 'prj_2', org_id: 'org_a', name: 'vision', created_at: at },
  ],
}

// What the API lists: the keys, never a token.
let stored: APIKey[]

function show() {
  const resolved = resolveTenant(me, '')
  if (resolved.state !== 'selected') throw new Error('no tenant')
  return render(
    <SWRConfig value={{ provider: () => new Map() }}>
      <TenantContext.Provider value={resolved.tenant}><APIKeys /></TenantContext.Provider>
    </SWRConfig>,
  )
}

async function create(name: string) {
  await userEvent.type(screen.getByLabelText('Key name'), name)
  await userEvent.click(screen.getByRole('button', { name: 'Create API key' }))
}

beforeEach(() => {
  stored = [{ id: 'k1', org_id: 'org_a', actor: { kind: 'user', id: 'u1', name: 'Ada' }, name: 'ci', created_at: at }]
  vi.mocked(fetchAPIKeys).mockImplementation(async () => [...stored])
  vi.mocked(createAPIKey).mockImplementation(async (orgID, name, projectID) => {
    const key: APIKey = { id: 'k2', org_id: orgID, project_id: projectID, actor: { kind: 'user', id: 'u1', name: 'Ada' }, name, created_at: at }
    stored = [key, ...stored]
    return { ...key, token: TOKEN }
  })
  vi.mocked(revokeAPIKey).mockImplementation(async id => {
    stored = stored.map(k => (k.id === id ? { ...k, revoked_at: at } : k))
    return stored.find(k => k.id === id)!
  })
})

describe('creating an API key', () => {
  it('shows the token once, with what it is for', async () => {
    show()
    await create('laptop')
    expect(createAPIKey).toHaveBeenCalledExactlyOnceWith('org_a', 'laptop', undefined)
    expect(await screen.findByTestId('secret')).toHaveProperty('textContent', TOKEN)
    expect(screen.getAllByText(TOKEN)).toHaveLength(1)
    expect(screen.getByText(/only time it is shown/)).toBeTruthy()
  })

  it('lists the new key without its token', async () => {
    show()
    await create('laptop')
    const row = (await screen.findByRole('cell', { name: 'laptop' })).closest('tr')!
    expect(within(row).getByText('whole organization')).toBeTruthy()
    expect(row.textContent).not.toContain(TOKEN)
  })

  it('copies the token to the clipboard', async () => {
    const user = userEvent.setup()
    show()
    await user.type(screen.getByLabelText('Key name'), 'laptop')
    await user.click(screen.getByRole('button', { name: 'Create API key' }))
    await user.click(await screen.findByRole('button', { name: 'Copy' }))
    expect(await navigator.clipboard.readText()).toBe(TOKEN)
    expect(screen.getByRole('button', { name: 'Copied' })).toBeTruthy()
  })

  it('drops the token for good once it is dismissed', async () => {
    show()
    await create('laptop')
    await userEvent.click(await screen.findByRole('button', { name: 'I have saved it' }))
    expect(screen.queryByTestId('secret')).toBeNull()
    expect(document.body.textContent).not.toContain(TOKEN)
    expect(screen.getByRole('cell', { name: 'laptop' })).toBeTruthy()
  })

  it('cannot bring the token back: leaving and returning reads the list, which holds no token', async () => {
    const first = show()
    await create('laptop')
    await screen.findByTestId('secret')
    first.unmount()

    show()
    await screen.findByRole('cell', { name: 'laptop' })
    expect(screen.queryByTestId('secret')).toBeNull()
    expect(document.body.textContent).not.toContain(TOKEN)
    expect(createAPIKey).toHaveBeenCalledTimes(1)
  })

  it('binds a key to one project when asked', async () => {
    show()
    await choose(screen.getByLabelText('Key reach'), 'Project vision only')
    await create('vision-bot')
    expect(createAPIKey).toHaveBeenCalledExactlyOnceWith('org_a', 'vision-bot', 'prj_2')
    const row = (await screen.findByRole('cell', { name: 'vision-bot' })).closest('tr')!
    expect(within(row).getByText('project vision')).toBeTruthy()
  })

  it('needs a name', async () => {
    show()
    expect(screen.getByRole('button', { name: 'Create API key' })).toHaveProperty('disabled', true)
    await userEvent.type(screen.getByLabelText('Key name'), '   ')
    expect(screen.getByRole('button', { name: 'Create API key' })).toHaveProperty('disabled', true)
  })

  it('shows the API\'s refusal and no token', async () => {
    vi.mocked(createAPIKey).mockRejectedValue(new Error('forbidden: only an owner of the org may change it'))
    show()
    await create('laptop')
    expect(await screen.findByText('forbidden: only an owner of the org may change it')).toBeTruthy()
    expect(screen.queryByTestId('secret')).toBeNull()
  })
})

describe('revoking an API key', () => {
  it('asks first, and does nothing when the answer is no', async () => {
    show()
    await userEvent.click(await screen.findByRole('button', { name: 'Revoke' }))
    const question = within(screen.getByRole('dialog'))
    expect(question.getByText('Revoke "ci"?')).toBeTruthy()
    await userEvent.click(question.getByRole('button', { name: 'Cancel' }))
    expect(screen.queryByRole('dialog')).toBeNull()
    expect(revokeAPIKey).not.toHaveBeenCalled()
  })

  it('revokes on yes and shows the key as revoked, with nothing left to revoke', async () => {
    show()
    await userEvent.click(await screen.findByRole('button', { name: 'Revoke' }))
    await userEvent.click(within(screen.getByRole('dialog')).getByRole('button', { name: 'Revoke' }))
    expect(revokeAPIKey).toHaveBeenCalledExactlyOnceWith('k1')
    await waitFor(() => expect(screen.getByText(/^revoked /)).toBeTruthy())
    expect(screen.queryByRole('button', { name: 'Revoke' })).toBeNull()
  })

  it('shows why a revoke failed', async () => {
    vi.mocked(revokeAPIKey).mockRejectedValue(new Error('api key not found'))
    show()
    await userEvent.click(await screen.findByRole('button', { name: 'Revoke' }))
    await userEvent.click(within(screen.getByRole('dialog')).getByRole('button', { name: 'Revoke' }))
    expect(await screen.findByText('api key not found')).toBeTruthy()
  })
})

describe('the key list', () => {
  it('says so when it cannot be loaded', async () => {
    vi.mocked(fetchAPIKeys).mockRejectedValue(new Error('internal error'))
    show()
    expect(await screen.findByText('Could not load API keys: internal error')).toBeTruthy()
  })
})

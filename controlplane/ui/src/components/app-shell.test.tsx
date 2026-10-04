import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { SWRConfig, useSWRConfig } from 'swr'
import { useState } from 'react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { ApiError, fetchMe, fetchPlatformExperiment } from '@/lib/api'
import { useTenant } from '@/lib/tenant'
import { AppShell } from '@/components/app-shell'
import type { Me, PlatformExperiment } from '@/types'

const navigation = vi.hoisted(() => ({ params: {} as Record<string, string> }))

vi.mock('next/navigation', () => ({ useParams: () => navigation.params }))
vi.mock('@/lib/api', async importOriginal => ({
  ApiError: (await importOriginal<typeof import('@/lib/api')>()).ApiError,
  fetchMe: vi.fn(),
  fetchPlatformExperiment: vi.fn(),
}))
vi.mock('@/components/nav', () => ({ Nav: () => <nav>nav</nav> }))
vi.mock('@/components/command-palette', async () => {
  const { useState } = await import('react')
  return {
    CommandPalette: () => {
      const [open, setOpen] = useState(false)
      return <button onClick={() => setOpen(true)}>{open ? 'palette open' : 'palette closed'}</button>
    },
  }
})

const at = '2026-01-01T00:00:00Z'
const me: Me = {
  kind: 'user',
  user_id: 'u1',
  name: 'Ada',
  orgs: [{ id: 'org_a', name: 'Ada', role: 'owner', created_at: at }],
  projects: [
    { id: 'prj_a1', org_id: 'org_a', name: 'default', created_at: at },
    { id: 'prj_a2', org_id: 'org_a', name: 'vision', created_at: at },
  ],
}

function Page() {
  const { org, project } = useTenant()
  const [token, setToken] = useState('')
  const { mutate } = useSWRConfig()
  return (
    <>
      <p>page of {org.name} / {project.name}</p>
      <button onClick={() => setToken('hls_shown')}>show token</button>
      <button onClick={() => void mutate('me')}>reread account</button>
      <output>{token}</output>
    </>
  )
}

function tree() {
  return (
    <SWRConfig value={{ provider: () => new Map(), shouldRetryOnError: false, dedupingInterval: 0 }}>
      <AppShell><Page /></AppShell>
    </SWRConfig>
  )
}

function show(params: Record<string, string> = {}) {
  navigation.params = params
  return render(tree())
}

beforeEach(() => {
  vi.mocked(fetchMe).mockResolvedValue(me)
})

describe('the signed-in shell', () => {
  it('renders nothing of the app until it knows who is signed in', () => {
    vi.mocked(fetchMe).mockReturnValue(new Promise(() => {}))
    show()
    expect(screen.getByRole('status', { name: 'Loading your account…' })).toBeTruthy()
    expect(screen.queryByText(/page of/)).toBeNull()
    expect(screen.queryByText('nav')).toBeNull()
  })

  it('renders the page inside the caller\'s home project', async () => {
    show()
    expect(await screen.findByText('page of Ada / default')).toBeTruthy()
    expect(screen.getByText('nav')).toBeTruthy()
  })

  it('says so when the API cannot be reached, and shows no empty app', async () => {
    vi.mocked(fetchMe).mockRejectedValue(new Error('Failed to fetch'))
    show()
    expect(await screen.findByText(/Cannot reach the API to load your account: Failed to fetch/)).toBeTruthy()
    expect(screen.queryByText(/page of/)).toBeNull()
  })

  it('says so when the URL names a project that is gone or out of reach, and offers the way home', async () => {
    show({ projectId: 'prj_deleted' })
    expect(await screen.findByText(/is not available to you/)).toBeTruthy()
    expect(screen.getByText('prj_deleted')).toBeTruthy()
    expect(screen.queryByText(/page of/)).toBeNull()
    expect(screen.getByRole('link', { name: 'Go to your experiments' }).getAttribute('href')).toBe('/console')
  })

  it('renders a page of an org in the first of its projects', async () => {
    show({ orgId: 'org_a' })
    expect(await screen.findByText('page of Ada / default')).toBeTruthy()
  })

  it('says so when the URL names an org the caller is not in', async () => {
    show({ orgId: 'org_other' })
    expect(await screen.findByText(/organization/)).toBeTruthy()
    expect(screen.getByText('org_other')).toBeTruthy()
    expect(screen.queryByText(/page of/)).toBeNull()
  })

  it('opens a page of an experiment in the experiment\'s own project, read from the experiment', async () => {
    vi.mocked(fetchPlatformExperiment).mockResolvedValue({ id: 'pe-1', project_id: 'prj_a2' } as PlatformExperiment)
    show({ experimentId: 'pe-1', jobId: 'j1' })
    expect(await screen.findByText('page of Ada / vision')).toBeTruthy()
    expect(fetchPlatformExperiment).toHaveBeenCalledWith('pe-1')
  })

  it('opens one shared by another org in the caller\'s own project', async () => {
    vi.mocked(fetchPlatformExperiment).mockResolvedValue({ id: 'pe-x', project_id: 'prj_of_another_org', shared: true } as PlatformExperiment)
    show({ experimentId: 'pe-x' })
    expect(await screen.findByText('page of Ada / default')).toBeTruthy()
  })

  it('says an experiment the caller cannot read was not found, and renders no page', async () => {
    vi.mocked(fetchPlatformExperiment).mockRejectedValue(new ApiError(404, 'platform experiment not found'))
    show({ experimentId: 'pe-gone' })
    expect(await screen.findByText(/was not found/)).toBeTruthy()
    expect(screen.getByText('pe-gone')).toBeTruthy()
    expect(screen.queryByText(/page of/)).toBeNull()
  })

  it('drops what a page and the palette held when the project changes', async () => {
    const view = show({ projectId: 'prj_a1' })
    await userEvent.click(await screen.findByRole('button', { name: 'show token' }))
    await userEvent.click(screen.getByRole('button', { name: 'palette closed' }))
    expect(screen.getByRole('status').textContent).toBe('hls_shown')
    expect(screen.getByRole('button', { name: 'palette open' })).toBeTruthy()

    navigation.params = { projectId: 'prj_a2' }
    view.rerender(tree())
    expect(await screen.findByText('page of Ada / vision')).toBeTruthy()
    expect(screen.getByRole('status').textContent).toBe('')
    expect(screen.getByRole('button', { name: 'palette closed' })).toBeTruthy()
  })

  it('keeps what a page holds while the project stays the same', async () => {
    const view = show({ projectId: 'prj_a1' })
    await userEvent.click(await screen.findByRole('button', { name: 'show token' }))
    view.rerender(tree())
    expect(screen.getByRole('status').textContent).toBe('hls_shown')
  })

  it('says so, and keeps the page, when a later reread of the account fails', async () => {
    show()
    expect(await screen.findByText('page of Ada / default')).toBeTruthy()
    vi.mocked(fetchMe).mockRejectedValue(new Error('Failed to fetch'))
    await userEvent.click(screen.getByRole('button', { name: 'reread account' }))
    expect(await screen.findByText(/Could not reread your account: Failed to fetch/)).toBeTruthy()
    expect(screen.getByText('page of Ada / default')).toBeTruthy()
  })

  it('says so when the caller reaches no project at all', async () => {
    vi.mocked(fetchMe).mockResolvedValue({ kind: 'user', orgs: [], projects: [] })
    show()
    expect(await screen.findByText(/Your account reaches no project/)).toBeTruthy()
  })
})

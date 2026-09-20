import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { SWRConfig } from 'swr'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { fetchMe } from '@/lib/api'
import { useTenant } from '@/lib/tenant'
import { AppShell } from '@/components/app-shell'
import type { Me } from '@/types'

const navigation = vi.hoisted(() => ({ push: vi.fn(), search: '' }))

vi.mock('next/navigation', () => ({
  useRouter: () => ({ push: navigation.push }),
  usePathname: () => '/jobs/j1',
  useSearchParams: () => new URLSearchParams(navigation.search),
}))
vi.mock('@/lib/api', () => ({ fetchMe: vi.fn() }))
vi.mock('@/components/nav', () => ({ Nav: () => <nav>nav</nav> }))
vi.mock('@/components/command-palette', () => ({ CommandPalette: () => null }))

const at = '2026-01-01T00:00:00Z'
const me: Me = {
  kind: 'user',
  user_id: 'u1',
  name: 'Ada',
  orgs: [{ id: 'org_a', name: 'Ada', role: 'owner', created_at: at }],
  projects: [{ id: 'prj_a1', org_id: 'org_a', name: 'default', created_at: at }],
}

function Page() {
  const { org, project } = useTenant()
  return <p>page of {org.name} / {project.name}</p>
}

function show(search = '') {
  navigation.search = search
  return render(
    <SWRConfig value={{ provider: () => new Map(), shouldRetryOnError: false }}>
      <AppShell><Page /></AppShell>
    </SWRConfig>,
  )
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
    show('project=prj_deleted')
    expect(await screen.findByText(/is not available to you/)).toBeTruthy()
    expect(screen.getByText('prj_deleted')).toBeTruthy()
    expect(screen.queryByText(/page of/)).toBeNull()
    await userEvent.click(screen.getByRole('button', { name: 'Go to Ada / default' }))
    expect(navigation.push).toHaveBeenCalledExactlyOnceWith('/jobs?project=prj_a1')
  })

  it('says so when the caller reaches no project at all', async () => {
    vi.mocked(fetchMe).mockResolvedValue({ kind: 'user', orgs: [], projects: [] })
    show()
    expect(await screen.findByText(/Your account reaches no project/)).toBeTruthy()
  })
})

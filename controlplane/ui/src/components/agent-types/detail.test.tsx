import { fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { SWRConfig } from 'swr'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { ApiError, archiveAgentType, createAgentType, editAgentType, fetchAgentType, fetchAgentTypeVersions } from '@/lib/api'
import { resolveTenant, TenantContext } from '@/lib/tenant'
import { AgentTypeDetail } from '@/components/agent-types/detail'
import { CATALOG, RESEARCHER_PROMPT, agentType } from '@/test-agent-types'
import { choose } from '@/test-select'
import type { AgentType, AgentTypeVersion, EditedAgentType, Me } from '@/types'

const navigation = vi.hoisted(() => ({ push: vi.fn() }))
vi.mock('next/navigation', () => ({ useRouter: () => navigation }))
vi.mock('@/lib/api', () => ({
  ApiError: class extends Error { constructor(readonly status: number, message: string, readonly reason?: string) { super(message) } },
  fetchAgentType: vi.fn(), fetchAgentTypeVersions: vi.fn(), editAgentType: vi.fn(), archiveAgentType: vi.fn(), createAgentType: vi.fn(),
}))

const at = '2026-01-01T00:00:00Z'
const me = (role: 'owner' | 'member'): Me => ({
  kind: 'user', user_id: 'u1', name: 'Ada',
  orgs: [{ id: 'org_a', name: 'Lab', role, created_at: at }],
  projects: [{ id: 'prj_1', org_id: 'org_a', name: 'default', created_at: at }],
})

let type: AgentType

function version(n: number, prompt: string, effort: AgentType['current']['settings']['effort'] = 'high'): AgentTypeVersion {
  return { agent_type: type.id, version: n, prompt, digest: `d${n}0000000000000`, created_at: at, created_by: { kind: 'user', id: 'u1', name: 'Ada' }, settings: { model: '', max_subagents: 4, pacing_subagents: 2, effort, hyperparameters: {} } }
}

function show(role: 'owner' | 'member' = 'owner') {
  const resolved = resolveTenant(me(role), '')
  if (resolved.state !== 'selected') throw new Error('no tenant')
  return render(
    <SWRConfig value={{ provider: () => new Map() }}>
      <TenantContext.Provider value={resolved.tenant}><AgentTypeDetail typeID={type.id} /></TenantContext.Provider>
    </SWRConfig>,
  )
}

const prompt = () => screen.getByRole('textbox', { name: 'Prompt' }) as HTMLTextAreaElement

function setPrompt(text: string) {
  fireEvent.change(prompt(), { target: { value: text } })
}

function edited(t: AgentType, moved: number, failed: EditedAgentType['propagation']['failed'] = []): EditedAgentType {
  return { ...t, propagation: { version: t.current_version, moved: Array.from({ length: moved }, (_, i) => ({ platform_experiment_id: 'pe-1', agent_id: `ag-${i}` })), failed } }
}

beforeEach(() => {
  navigation.push.mockReset()
  type = CATALOG.find(t => t.id === 'at-lr')!
  vi.mocked(fetchAgentType).mockImplementation(async () => type)
  vi.mocked(fetchAgentTypeVersions).mockImplementation(async () => [version(3, RESEARCHER_PROMPT, 'max'), version(2, RESEARCHER_PROMPT.replace('researcher', 'tuner')), version(1, 'Old.\n{{rules}}\n{{hl_commands}}')])
})

describe('editing an org\'s agent type', () => {
  it('shows each placeholder the role may hold with what it is, and inserts one at the cursor', async () => {
    show()
    await screen.findByRole('textbox', { name: 'Prompt' })
    const chips = within(screen.getByRole('group', { name: 'Placeholders' }))
    expect(chips.getByText("The platform's rules every agent is bound by.")).toBeTruthy()
    expect(chips.getByRole('button', { name: 'Insert {{hyperparameters}}' })).toBeTruthy()
    setPrompt('{{rules}} {{hl_commands}} ')
    prompt().setSelectionRange(prompt().value.length, prompt().value.length)
    await userEvent.click(chips.getByRole('button', { name: 'Insert {{workdir}}' }))
    expect(prompt().value).toBe('{{rules}} {{hl_commands}} {{workdir}}')
  })

  it('says what the API would refuse in the prompt as it is typed, and holds the save', async () => {
    show()
    await screen.findByRole('textbox', { name: 'Prompt' })
    setPrompt('No rules here {{secret}}')
    expect(screen.getByText('{{secret}} has no value in a researcher prompt.')).toBeTruthy()
    expect(screen.getByText('{{rules}} is required.')).toBeTruthy()
    expect(screen.getByText('{{hl_commands}} is required.')).toBeTruthy()
    expect(screen.getByText('3 problems to fix before saving.')).toBeTruthy()
    expect((screen.getByRole('button', { name: 'Save as v4' }) as HTMLButtonElement).disabled).toBe(true)
  })

  it('asks before an edit that restarts live agents\' sessions, then saves from the version it was made on and says who moved', async () => {
    const saved = agentType('at-lr', 'LR sweeper', 'researcher', { current_version: 4, live_agents: 2 })
    vi.mocked(editAgentType).mockImplementation(async () => { type = saved; return edited(saved, 2) })
    show()
    await screen.findByRole('textbox', { name: 'Prompt' })
    await choose(screen.getByLabelText('Max subagents'), '6')
    expect(screen.getByText('Saving makes v4. 2 live agents move to it.')).toBeTruthy()
    await userEvent.click(screen.getByRole('button', { name: 'Save as v4' }))
    const dialog = within(screen.getByRole('dialog'))
    expect(dialog.getByText(/2 live agents run this type\. Each one restarts its session with the new version at its next turn/)).toBeTruthy()
    expect(editAgentType).not.toHaveBeenCalled()
    await userEvent.click(dialog.getByRole('button', { name: 'Save and restart sessions' }))
    expect(editAgentType).toHaveBeenCalledExactlyOnceWith('org_a', 'at-lr', {
      if_version: 3, name: 'LR sweeper', prompt: RESEARCHER_PROMPT,
      settings: { model: '', max_subagents: 6, pacing_subagents: 2, effort: 'high', hyperparameters: {} },
    })
    expect(await screen.findByText(/Saved as v4\. 2 live agents pick it up at their next turn\./)).toBeTruthy()
  })

  it('names the agents an edit could not move, which keep their version', async () => {
    const saved = agentType('at-lr', 'LR sweeper', 'researcher', { current_version: 4 })
    vi.mocked(editAgentType).mockImplementation(async () => { type = saved; return edited(saved, 1, [{ platform_experiment_id: 'pe-9', agent_id: 'ag-x', error: 'model refused' }]) })
    type = { ...type, live_agents: 0 }
    show()
    await screen.findByRole('textbox', { name: 'Prompt' })
    setPrompt(`${RESEARCHER_PROMPT}More.`)
    await userEvent.click(screen.getByRole('button', { name: 'Save as v4' }))
    expect(await screen.findByText('1 could not be moved and keep the version they had:')).toBeTruthy()
    expect(screen.getByText(/: model refused/)).toBeTruthy()
  })

  it('renames without asking, since a name makes no version', async () => {
    vi.mocked(editAgentType).mockImplementation(async () => { type = { ...type, name: 'LR' }; return edited(type, 0) })
    show()
    const name = await screen.findByLabelText('Name')
    await userEvent.clear(name)
    await userEvent.type(name, 'LR')
    expect(screen.getByText('Only the name changed: no new version.')).toBeTruthy()
    await userEvent.click(screen.getByRole('button', { name: 'Save' }))
    expect(screen.queryByRole('dialog')).toBeNull()
    expect(await screen.findByText('Renamed. A new name makes no new version.')).toBeTruthy()
  })

  it('says another save came first, and offers to reload', async () => {
    vi.mocked(editAgentType).mockRejectedValue(new ApiError(409, 'version 3 is not current: 4 is', 'version_conflict'))
    type = { ...type, live_agents: 0 }
    show()
    await screen.findByRole('textbox', { name: 'Prompt' })
    setPrompt(`${RESEARCHER_PROMPT}More.`)
    await userEvent.click(screen.getByRole('button', { name: 'Save as v4' }))
    expect(await screen.findByText(/Someone saved a newer version while you were editing\. version 3 is not current/)).toBeTruthy()
    expect(screen.getByRole('button', { name: 'Reload' })).toBeTruthy()
  })
})

describe('what nobody here edits', () => {
  it('shows a built-in type read-only with why, and offers a copy', async () => {
    type = CATALOG[0]
    vi.mocked(createAgentType).mockResolvedValue(agentType('at-copy', 'Copy of Generalist', 'researcher'))
    show()
    expect(await screen.findByText(/Built-in types are read-only for everyone/)).toBeTruthy()
    expect(prompt().readOnly).toBe(true)
    expect(screen.queryByRole('button', { name: 'Archive' })).toBeNull()
    await userEvent.click(screen.getByRole('button', { name: 'Duplicate' }))
    expect(createAgentType).toHaveBeenCalledExactlyOnceWith('org_a', { from: 'generalist' })
    expect(navigation.push).toHaveBeenCalledWith('/orgs/org_a/settings/agent-types/at-copy')
  })

  it('shows a member the org\'s type read-only and says who can edit it', async () => {
    show('member')
    expect(await screen.findByText('Only an owner of Lab can edit agent types. You can read this one.')).toBeTruthy()
    expect(prompt().readOnly).toBe(true)
    expect(screen.queryByRole('button', { name: /Duplicate|Archive/ })).toBeNull()
  })
})

describe('archiving', () => {
  it('says plainly why a type in use stays', async () => {
    vi.mocked(archiveAgentType).mockRejectedValue(new ApiError(409, 'pe-1 names it', 'agent_type_in_use'))
    show()
    await userEvent.click(await screen.findByRole('button', { name: 'Archive' }))
    const dialog = within(screen.getByRole('dialog'))
    await userEvent.click(dialog.getByRole('button', { name: 'Archive' }))
    expect(await dialog.findByText(/Not archived: an experiment that is not closed still uses it/)).toBeTruthy()
    expect(dialog.queryByRole('button', { name: 'Archive' })).toBeNull()
  })

  it('archives one nobody uses', async () => {
    vi.mocked(archiveAgentType).mockImplementation(async () => { type = { ...type, archived_at: at }; return type })
    show()
    await userEvent.click(await screen.findByRole('button', { name: 'Archive' }))
    await userEvent.click(within(screen.getByRole('dialog')).getByRole('button', { name: 'Archive' }))
    expect(archiveAgentType).toHaveBeenCalledExactlyOnceWith('org_a', 'at-lr')
    await waitFor(() => expect(screen.getByText(/It takes no new agents and can no longer be edited/)).toBeTruthy())
  })
})

describe('version history', () => {
  it('lists every version and shows what changed between any two', async () => {
    show()
    expect(await screen.findByText('3 versions')).toBeTruthy()
    expect(screen.getByText('Effort:')).toBeTruthy()
    const diff = screen.getByLabelText('Prompt diff')
    expect(within(diff).getByText('You are a tuner.')).toBeTruthy()
    expect(within(diff).getByText('You are a researcher.')).toBeTruthy()
    await choose(screen.getByLabelText('Compare from'), 'v1')
    expect(within(screen.getByLabelText('Prompt diff')).getByText('Old.')).toBeTruthy()
  })
})

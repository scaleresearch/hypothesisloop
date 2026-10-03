// @vitest-environment jsdom
import { afterEach, describe, expect, it } from 'vitest'
import { cleanup, render, screen } from '@testing-library/react'
import type { Actor as ActorRecord } from '@/types'
import { Actor, Person, Verdict, actorLabel, verdictBy } from '@/components/actor'

afterEach(cleanup)

const ada: ActorRecord = { kind: 'user', id: 'u1', name: 'Ada Lovelace' }
const key: ActorRecord = { kind: 'key', id: 'key_1', name: 'ci' }
const agent: ActorRecord = { kind: 'agent', id: 'ag-1', name: 'ag-1' }
const staff: ActorRecord = { kind: 'staff', id: '', name: 'staff' }
const coordinator: ActorRecord = { kind: 'coordinator', id: 'pe-1', name: 'coordinator' }
const typed: ActorRecord = { kind: 'unverified', id: '', name: 'Mallory' }

describe('who made a record, in words', () => {
  it.each([
    ['a person', ada, '', 'Ada Lovelace'],
    ['a key', key, '', 'key ci'],
    ['staff', staff, '', 'staff'],
    ['a coordinator', coordinator, '', 'coordinator'],
    ['a name typed before actors were kept', typed, '', 'Mallory (unverified)'],
    ['an agent with its own token', agent, 'ag-1', 'ag-1'],
    ['an agent\'s record from before actors were kept', { ...typed, name: 'ag-1' }, 'ag-1', 'ag-1'],
    ['a person writing for an agent', ada, 'ag-1', 'ag-1 (by Ada Lovelace)'],
    ['one agent writing for another', { ...agent, id: 'ag-2', name: 'ag-2' }, 'ag-1', 'ag-1 (by ag-2)'],
  ])('%s', (_, actor, agentID, want) => {
    expect(actorLabel(actor, agentID)).toBe(want)
  })

  it('for a verdict, with nothing while none was given', () => {
    expect(verdictBy({ agent_id: '', status_actor: null, status_changed_at: null })).toBe('')
    expect(verdictBy({ agent_id: '', status_actor: ada, status_changed_at: new Date().toISOString() })).toMatch(/^by Ada Lovelace · /)
    expect(verdictBy({ agent_id: 'ag-1', status_actor: key, status_changed_at: new Date().toISOString() })).toMatch(/^by ag-1 \(by key ci\) · /)
  })
})

describe('the actor of a record', () => {
  it('is a person marked human, by name with the account id on hover', () => {
    render(<Actor actor={ada} />)
    expect(screen.getByText('human')).toBeTruthy()
    expect(screen.getByText('Ada Lovelace').getAttribute('title')).toBe('u1')
  })

  it('is a key marked as one, so its writes are not read as a person\'s', () => {
    render(<Actor actor={key} />)
    expect(screen.getByText('key')).toBeTruthy()
    expect(screen.getByText('ci').getAttribute('title')).toBe('key_1')
    expect(screen.queryByText('human')).toBeNull()
  })

  it('says a name from before actors were kept is unverified', () => {
    render(<Actor actor={typed} />)
    expect(screen.getByText('Mallory')).toBeTruthy()
    expect(screen.getByText('unverified').closest('[title]')!.getAttribute('title')).toContain('no account stands behind it')
    expect(screen.queryByText('human')).toBeNull()
  })

  it('is the agent alone when the agent wrote its own record', () => {
    const { container } = render(<Actor actor={agent} agentID="ag-1" />)
    expect(container.textContent).toBe('ag-1')
  })

  it('names the agent first and then who wrote for it', () => {
    const { container } = render(<Actor actor={ada} agentID="ag-1" />)
    expect(container.textContent).toBe('ag-1byhumanAda Lovelace')
  })

  it('fits a graph node as one line of text', () => {
    render(<Actor actor={ada} agentID="ag-1" compact />)
    expect(screen.getByTitle('ag-1 (by Ada Lovelace)').textContent).toBe('ag-1 (by Ada Lovelace)')
  })
})

describe('a verdict', () => {
  it('names who gave it', () => {
    const { container } = render(<Verdict hypothesis={{ status: 'confirmed', agent_id: '', status_actor: staff, status_changed_at: new Date().toISOString() }} />)
    expect(container.textContent).toMatch(/^confirmed by staff · /)
  })

  it('is nothing while none was given', () => {
    const { container } = render(<Verdict hypothesis={{ status: 'open', agent_id: 'ag-1', status_actor: null, status_changed_at: null }} />)
    expect(container.textContent).toBe('')
  })
})

describe('a person of an org', () => {
  it('is shown by name, and by id while they carry none', () => {
    render(<><Person id="u1" name="Ada Lovelace" self /><Person id="u2" name="" self={false} /></>)
    expect(screen.getByText('Ada Lovelace').getAttribute('title')).toBe('u1')
    expect(screen.getAllByText('you')).toHaveLength(1)
    expect(screen.getByText('u2')).toBeTruthy()
  })
})

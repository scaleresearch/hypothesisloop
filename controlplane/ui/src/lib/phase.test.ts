import { readFileSync } from 'node:fs'
import { join } from 'node:path'
import { describe, expect, it } from 'vitest'
import { OWNER_ACTIONS } from '@/lib/phase'

// The platform's one table of who each status reason waits for.
const STATUS_REASONS = join(__dirname, '../../../domain/status_reason.go')

function waiters(): Map<string, string> {
  const table = readFileSync(STATUS_REASONS, 'utf8').match(/var statusWaiters = map\[string\]Waiter\{([\s\S]*?)\n\}/)
  if (!table) throw new Error(`${STATUS_REASONS} has no statusWaiters table`)
  return new Map(Array.from(table[1].matchAll(/"(\w+)":\s+WaitsFor(\w+),/g), ([, reason, who]) => [reason, who]))
}

// A reason the platform began to answer with showed the owner a callout with nothing to press,
// or one with a button for something else.
describe('the page\'s table of what it offers the owner', () => {
  it('reads the platform\'s table', () => {
    expect(waiters().get('planning')).toBe('Platform')
    expect(waiters().size).toBeGreaterThan(15)
  })

  it('has an entry for every reason that can wait for the owner, and for no other', () => {
    // A coordinator's move is the owner's where a person coordinates (domain.WaitsFor).
    const ownerMay = Array.from(waiters()).filter(([, who]) => who === 'Owner' || who === 'Coordinator').map(([reason]) => reason)
    expect(Object.keys(OWNER_ACTIONS).sort()).toEqual(ownerMay.sort())
  })

  it('offers the budget where the budget is the matter and the agents card where an executor is', () => {
    expect(OWNER_ACTIONS.budget_spent).toEqual({ opens: 'edit', label: 'Edit budget' })
    expect(OWNER_ACTIONS.budget_below_baseline).toEqual({ opens: 'edit', label: 'Edit budget' })
    expect(OWNER_ACTIONS.no_executors).toEqual({ opens: 'agents', label: 'Add an executor' })
  })
})

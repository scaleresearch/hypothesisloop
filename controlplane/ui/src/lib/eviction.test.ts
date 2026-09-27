import { describe, expect, it } from 'vitest'
import { notAdmittedLabel } from './eviction'

describe('notAdmittedLabel', () => {
  it('follows the label of the code with the sentence of the scheduler', () => {
    expect(notAdmittedLabel('capacity_unavailable', 'cluster dev is short of 2 cores, held by 3 jobs'))
      .toBe('No capacity available: cluster dev is short of 2 cores, held by 3 jobs')
  })

  it('is the label alone where the scheduler wrote no sentence', () => {
    expect(notAdmittedLabel('summary_gate')).toBe('Held until its agent files the summaries it owes for finished jobs')
  })

  it('shows a code it has no label for as it is', () => {
    expect(notAdmittedLabel('a_new_code')).toBe('a_new_code')
  })
})

import { describe, expect, it } from 'vitest'
import { evictionCodeLabel, evictionDetailLines, evictionLabel, notAdmittedLabel } from './eviction'

describe('notAdmittedLabel', () => {
  it('follows the label of the code with the sentence of the scheduler', () => {
    expect(notAdmittedLabel('capacity_unavailable', 'cluster dev is short of 2 cores, held by 3 jobs'))
      .toBe('No capacity available: cluster dev is short of 2 cores, held by 3 jobs')
  })

  it('follows the summary gate with the jobs owed and the command that files a summary', () => {
    expect(notAdmittedLabel('summary_gate', 'job job-1 of this agent is COMPLETED and has no summary: hl job summary job-1 --text T files one'))
      .toBe('Held until its agent files the summary of a job that ended before this one was submitted: job job-1 of this agent is COMPLETED and has no summary: hl job summary job-1 --text T files one')
  })

  it('shows a code it has no label for as it is', () => {
    expect(notAdmittedLabel('a_new_code')).toBe('a_new_code')
  })
})

describe('a reason a job was requeued or evicted for', () => {
  it.each(['code_unreachable', 'no_offer'])('labels %s as a fault of the platform at no cost', code => {
    expect(evictionCodeLabel(code)).toMatch(/\(platform fault, no cost\)$/)
  })

  it('follows the label with the sentence the platform wrote', () => {
    expect(evictionLabel('no_offer: no machine to rent for shape rtx4090 in 3m0s of asking'))
      .toBe('No machine was on offer to rent (platform fault, no cost) — no machine to rent for shape rtx4090 in 3m0s of asking')
  })
})

describe('evictionDetailLines', () => {
  it('gives the last lines a failed job said one per row', () => {
    expect(evictionDetailLines('container_failed: command exited with code 1: Traceback | ValueError: bad shape | see the docs'))
      .toEqual(['command exited with code 1: Traceback', 'ValueError: bad shape', 'see the docs'])
  })

  it('is empty for a reason that is a code alone', () => {
    expect(evictionDetailLines('stage_cut')).toEqual([])
  })
})

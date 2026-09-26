'use client'

import { useState } from 'react'
import { createDraft, MAX_GOAL_LENGTH } from '@/lib/planning'
import type { Draft } from '@/lib/planning'
import { Button } from '@/components/ui/button'
import { FormField } from '@/components/ui/form-field'
import { Textarea } from '@/components/ui/input'
import { InlineError } from '@/components/ui/status-message'
import { ConnectGitHubStep, useGitHub } from '@/components/connect-github-step'
import { RepositorySelect, repositoryFields } from '@/components/repository-select'

/**
 * The shortest way to an experiment: one sentence saying what to find out. The platform plans
 * the rest (the metric, a measured baseline, the brief, the cost) and proposes it on the draft's
 * page, where the person accepts it and states what the run may spend.
 */
export function GoalInput({ projectID, onCreated }: { projectID: string; onCreated: (draft: Draft) => void }) {
  const [goal, setGoal] = useState('')
  const [sending, setSending] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const github = useGitHub()
  const [repository, setRepository] = useState('')

  async function plan() {
    setSending(true)
    setError(null)
    try {
      onCreated(await createDraft(projectID, goal.trim(), repositoryFields(repository)))
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
      setSending(false)
    }
  }

  return (
    <div className="grid gap-2">
      <FormField
        label="What do you want to find out?"
        hint="One sentence is enough. The platform works out how to measure it, runs a baseline and proposes the experiment in about half an hour. Nothing is spent on the run until you accept."
      >
        <Textarea
          rows={3}
          maxLength={MAX_GOAL_LENGTH}
          value={goal}
          onChange={e => setGoal(e.target.value)}
          placeholder="e.g. Does a cosine learning-rate schedule beat a constant one for a small transformer on character-level text?"
        />
      </FormField>
      {github === 'connected' && (
        <FormField label="Repository" hint="Where the planner and the agents keep their code, each on its own branch. Fixed once created.">
          <RepositorySelect value={repository} onChange={setRepository} />
        </FormField>
      )}
      {github === 'unconnected' ? <ConnectGitHubStep /> : (
        <div className="flex items-center gap-3">
          <Button variant="primary" loading={sending} disabled={goal.trim() === ''} onClick={plan}>Plan this experiment</Button>
          {error && <InlineError>{error}</InlineError>}
        </div>
      )}
    </div>
  )
}

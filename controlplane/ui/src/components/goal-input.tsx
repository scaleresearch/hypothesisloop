'use client'

import { useState } from 'react'
import { repositoryNotProvided } from '@/lib/api'
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
  // What the API said of a new repository GitHub would not give it, shown where one is chosen.
  const [notCreated, setNotCreated] = useState<string | null>(null)
  const github = useGitHub()
  const [repository, setRepository] = useState('')

  // One rule for the button and for Enter: a sentence, GitHub connected where it is needed, and no plan already on its way.
  const ready = goal.trim() !== '' && !sending && github !== 'unconnected'

  async function plan() {
    if (!ready) return
    setSending(true)
    setError(null)
    setNotCreated(null)
    try {
      onCreated(await createDraft(projectID, goal.trim(), repositoryFields(repository)))
    } catch (e) {
      if (repositoryNotProvided(e)) setNotCreated(e.message)
      else setError(e instanceof Error ? e.message : String(e))
      setSending(false)
    }
  }

  return (
    <div className="grid gap-2">
      <FormField
        label="What do you want to find out?"
        hint="One sentence is enough. Enter plans it, Shift+Enter starts a new line. The platform works out how to measure it and proposes the experiment in a few minutes. Nothing runs and nothing is spent until you accept."
      >
        <Textarea
          rows={3}
          maxLength={MAX_GOAL_LENGTH}
          value={goal}
          onChange={e => setGoal(e.target.value)}
          onKeyDown={e => {
            if (e.key !== 'Enter' || e.shiftKey || e.nativeEvent.isComposing) return
            e.preventDefault()
            void plan()
          }}
          placeholder="e.g. Does a cosine learning-rate schedule beat a constant one for a small transformer on character-level text?"
        />
      </FormField>
      {github === 'connected' && (
        <FormField label="Repository" hint="Where the planner and the agents keep their code, each on its own branch. Fixed once created.">
          <RepositorySelect value={repository} onChange={r => { setRepository(r); setNotCreated(null) }} notCreated={notCreated ?? undefined} />
        </FormField>
      )}
      {github === 'unconnected' ? <ConnectGitHubStep /> : (
        <div className="flex items-center gap-3">
          <Button variant="primary" loading={sending} disabled={goal.trim() === ''} onClick={plan} aria-keyshortcuts="Enter">Plan this experiment</Button>
          {error && <InlineError>{error}</InlineError>}
        </div>
      )}
    </div>
  )
}

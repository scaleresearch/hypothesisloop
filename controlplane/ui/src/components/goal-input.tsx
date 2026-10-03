'use client'

import { useRef, useState } from 'react'
import { Field, Label } from '@headlessui/react'
import { repositoryNotProvided } from '@/lib/api'
import { GITHUB_CARD } from '@/lib/github-connect'
import { matchingGoals } from '@/lib/goal-examples'
import { createDraft, MAX_GOAL_LENGTH } from '@/lib/planning'
import type { Draft } from '@/lib/planning'
import { Button } from '@/components/ui/button'
import { Textarea } from '@/components/ui/input'
import { TextLink } from '@/components/ui/link'
import { InlineError } from '@/components/ui/status-message'
import { useGitHub } from '@/components/connect-github-step'
import { RepositorySelect, repositoryFields } from '@/components/repository-select'

const CONNECT_FIRST = 'Connect GitHub in Settings to start'
const ASK = 'What do you want to find out?'

/**
 * The shortest way to an experiment: one sentence saying what to find out. The platform plans
 * the rest and proposes it on the draft's page, where the person approves it.
 */
export function GoalInput({ projectID, onCreated }: { projectID: string; onCreated: (draft: Draft) => void }) {
  const [goal, setGoal] = useState('')
  const [sending, setSending] = useState(false)
  const [error, setError] = useState<string | null>(null)
  // What the API said of a new repository GitHub would not give it, shown where one is chosen.
  const [notCreated, setNotCreated] = useState<string | null>(null)
  const github = useGitHub()
  const [repository, setRepository] = useState('')
  const box = useRef<HTMLTextAreaElement>(null)
  const suggestions = matchingGoals(goal)

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
    <div className="grid gap-2.5">
      <Textarea
        ref={box}
        rows={2}
        maxLength={MAX_GOAL_LENGTH}
        aria-label={ASK}
        placeholder={ASK}
        // The page's styled scrollbars give an empty textarea a horizontal one; the sentence wraps instead.
        className="w-full overflow-x-hidden break-words"
        value={goal}
        onChange={e => setGoal(e.target.value)}
        onKeyDown={e => {
          if (e.key !== 'Enter' || e.shiftKey || e.nativeEvent.isComposing) return
          e.preventDefault()
          void plan()
        }}
      />
      {suggestions.length > 0 && (
        <ul aria-label="Suggestions" className="flex min-w-0 flex-wrap gap-1.5">
          {suggestions.map(example => (
            <li key={example} className="min-w-0 max-w-full">
              <button
                type="button"
                title={example}
                className="block max-w-full truncate rounded-full border border-border-control px-2.5 py-0.5 text-xs text-muted transition-colors hover:bg-hover hover:text-foreground"
                onClick={() => { setGoal(example); box.current?.focus() }}
              >
                {example}
              </button>
            </li>
          ))}
        </ul>
      )}
      {github === 'unconnected' && (
        <p className="text-xs text-muted">
          <TextLink inline href={`/settings#${GITHUB_CARD}`}>Connect GitHub in Settings</TextLink> to start.
        </p>
      )}
      <div className="flex flex-wrap items-center gap-3">
        {/* A disabled button shows no tooltip in every browser, so the reason sits on its wrapper. */}
        <span title={github === 'unconnected' ? CONNECT_FIRST : undefined}>
          <Button variant="primary" loading={sending} disabled={goal.trim() === '' || github === 'unconnected'} onClick={plan} aria-keyshortcuts="Enter">
            Plan it
          </Button>
        </span>
        {github === 'connected' && (
          <Field className="grid gap-1">
            <Label className="sr-only">Repository</Label>
            <RepositorySelect compact value={repository} onChange={r => { setRepository(r); setNotCreated(null) }} notCreated={notCreated ?? undefined} />
          </Field>
        )}
        {error && <InlineError>{error}</InlineError>}
      </div>
    </div>
  )
}

'use client'

import { useLayoutEffect, useRef, useState } from 'react'
import useSWR from 'swr'
import { Field, Label } from '@headlessui/react'
import { ApiError, GITHUB_NOT_CONFIGURED, fetchHosting } from '@/lib/api'
import type { ExperimentDraft } from '@/lib/api'
import { useGitHubApp } from '@/lib/github-connect'
import { createDraft, formDraftFromGoal, hostingRefusal, MAX_GOAL_LENGTH } from '@/lib/planning'
import type { Draft } from '@/lib/planning'
import type { StatusReason } from '@/types'
import { BriefGuide } from '@/components/brief-guide'
import { InstallGitHub, useGitHub } from '@/components/connect-github-step'
import { HostingNotice } from '@/components/hosting-notice'
import { RepositorySelect, repositoryFields } from '@/components/repository-select'
import { Button } from '@/components/ui/button'
import { Textarea } from '@/components/ui/input'
import { Inline } from '@/components/ui/stack'
import { InlineError } from '@/components/ui/status-message'
import { errorMessage } from '@/lib/errors'

const ASK = 'What do you want to find out?'
const PLACEHOLDER = `${ASK} For example: does a cosine learning-rate schedule beat a constant one for train.py, judged by validation loss?`

/**
 * The shortest way to an experiment: a few sentences saying what to find out. The platform plans
 * the rest and proposes it on the draft's page, where the person approves it. While the platform
 * cannot start a planner, it says so before anything is made, and the text goes on to the full
 * form for a person who sets it up themselves.
 */
export function GoalInput({ projectID, onCreated, onConfigure }: {
  projectID: string
  onCreated: (draft: Draft) => void
  onConfigure: (draft: ExperimentDraft) => void
}) {
  const [goal, setGoal] = useState('')
  const [sending, setSending] = useState(false)
  const [moving, setMoving] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [refused, setRefused] = useState<StatusReason | null>(null)
  const github = useGitHub()
  const app = useGitHubApp()
  const plainHost = app.error instanceof ApiError && app.error.reason === GITHUB_NOT_CONFIGURED
  // Read again while the page is open, so that planning is offered again once it can start.
  const hosting = useSWR(['hosting'], fetchHosting, { refreshInterval: 15_000, onSuccess: h => { if (h.available) setRefused(null) } })
  const unavailable = hosting.data && !hosting.data.available ? hosting.data.unavailable ?? null : refused
  const [repository, setRepository] = useState('')
  const box = useRef<HTMLTextAreaElement>(null)

  useLayoutEffect(() => {
    const el = box.current
    if (!el || el.scrollHeight === 0) return
    el.style.height = 'auto'
    el.style.height = `${el.scrollHeight}px`
  }, [goal])

  // One rule for the button and for Enter.
  const ready = goal.trim() !== '' && !sending && github !== 'unconnected' && !unavailable

  async function plan() {
    if (!ready) return
    setSending(true)
    setError(null)
    try {
      onCreated(await createDraft(projectID, goal.trim(), repositoryFields(repository)))
    } catch (e) {
      const hostingReason = hostingRefusal(e)
      if (hostingReason) {
        setRefused(hostingReason)
        void hosting.mutate()
      } else setError(errorMessage(e))
      setSending(false)
    }
  }

  async function configure() {
    setMoving(true)
    setError(null)
    try {
      onConfigure(await formDraftFromGoal(projectID, goal.trim(), repository))
    } catch (e) {
      setError(`Could not save your text as a draft: ${errorMessage(e)}`)
    }
    setMoving(false)
  }

  return (
    <div>
      <Textarea
        ref={box}
        bare
        rows={3}
        maxLength={MAX_GOAL_LENGTH}
        aria-label={ASK}
        placeholder={PLACEHOLDER}
        // The page's styled scrollbars give an empty textarea a horizontal one; the sentence wraps instead.
        className="block max-h-[60vh] w-full resize-none overflow-y-auto overflow-x-hidden break-words px-6 pb-2 pt-5 text-lg"
        value={goal}
        onChange={e => setGoal(e.target.value)}
        onKeyDown={e => {
          if (e.key !== 'Enter' || e.shiftKey || e.nativeEvent.isComposing) return
          e.preventDefault()
          void plan()
        }}
      />
      <BriefGuide note="Anything you leave out, the planner proposes, and you review the whole plan before anything runs or is spent." className="px-6 pb-3" />
      {unavailable && (
        <div className="px-4 pb-3">
          <HostingNotice
            reason={unavailable}
            title="Planning can't start right now"
            meaning="Your text stays here. Planning becomes available again by itself, or you can set the experiment up yourself and keep what you wrote as its brief."
            actions={<Button size="sm" loading={moving} onClick={() => void configure()}>Set it up myself</Button>}
          />
        </div>
      )}
      {hosting.error && <InlineError className="block px-6 pb-3">Could not check whether planning is available: {hosting.error.message}</InlineError>}
      <Inline gap="md" className="px-4 pb-4">
        {(github === 'connected' || plainHost) && (
          <Field className="flex min-w-0 items-center gap-2 pl-2">
            <Label className="text-sm text-muted">Repository</Label>
            <RepositorySelect compact value={repository} onChange={setRepository} />
          </Field>
        )}
        {github === 'unconnected' && (
          <Inline gap="sm" className="px-2"><InstallGitHub /></Inline>
        )}
        {error && <InlineError className="px-2">{error}</InlineError>}
        <span className="ml-auto">
          <Button variant="primary" loading={sending} disabled={!ready && !sending} onClick={plan} aria-keyshortcuts="Enter">
            Plan it
          </Button>
        </span>
      </Inline>
    </div>
  )
}

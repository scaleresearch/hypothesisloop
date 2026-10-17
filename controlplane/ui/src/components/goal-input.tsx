'use client'

import { useEffect, useLayoutEffect, useRef, useState } from 'react'
import useSWR from 'swr'
import { Field, Label } from '@headlessui/react'
import { GITHUB_NOT_CONFIGURED } from '@/lib/api/github'
import { fetchHosting } from '@/lib/api/hosting'
import { createDraft } from '@/lib/api/planning'
import { ApiError } from '@/lib/api/request'
import { formDraftFromGoal, hostingRefusal, MAX_GOAL_LENGTH } from '@/lib/planning'
import { useGitHubApp } from '@/lib/github-connect'
import { GOAL_EXAMPLES } from '@/lib/goal-examples'
import { BriefGuide } from '@/components/brief-guide'
import { InstallGitHub, useGitHub } from '@/components/connect-github-step'
import { RepositorySelect, repositoryFields } from '@/components/repository-select'
import { Button } from '@/components/ui/button'
import { Textarea } from '@/components/ui/input'
import { Inline } from '@/components/ui/stack'
import { Callout, InlineError } from '@/components/ui/status-message'
import { Text } from '@/components/ui/text'
import { errorMessage } from '@/lib/errors'
import { keys } from '@/lib/keys'
import { POLL } from '@/lib/poll'
import type { ExperimentDraft, PlatformExperiment, StatusReason } from '@/types/api'

const ASK = 'What do you want to find out?'
const CONNECT_FIRST = 'Install the GitHub App to start'
const CANNOT_START = "Planning can't start right now"

// The rhythm of the examples typed in the empty box, in milliseconds.
const FIRST = 1500
const TYPE = 42
const ERASE = 16
const HOLD = 2400
const GAP = 450

/**
 * The placeholder of the empty box: the question, erased, then one example typed after another,
 * each held, erased and followed by the next. It stops for good, back on the question, once the
 * person takes the box, and never starts for one who asks for reduced motion.
 */
function useTypedExamples(stopped: boolean): string {
  const [shown, setShown] = useState(ASK)
  useEffect(() => {
    if (stopped || window.matchMedia?.('(prefers-reduced-motion: reduce)').matches) return
    let text = ASK
    let length = ASK.length
    let next = 0
    let timer: ReturnType<typeof setTimeout>
    const erase = () => {
      length -= 1
      setShown(text.slice(0, length))
      if (length > 0) timer = setTimeout(erase, ERASE)
      else {
        text = GOAL_EXAMPLES[next]
        next = (next + 1) % GOAL_EXAMPLES.length
        timer = setTimeout(type, GAP)
      }
    }
    const type = () => {
      length += 1
      setShown(text.slice(0, length))
      timer = setTimeout(length < text.length ? type : erase, length < text.length ? TYPE : HOLD)
    }
    timer = setTimeout(erase, FIRST)
    return () => {
      clearTimeout(timer)
      setShown(ASK)
    }
  }, [stopped])
  return shown
}

/**
 * The shortest way to an experiment: a few sentences saying what to find out. The platform plans
 * the rest and proposes it on the draft's page, where the person approves it. While the platform
 * cannot start a planner, it says so before anything is made, and the text goes on to the full
 * form for a person who sets it up themselves.
 */
export function GoalInput({ projectID, onCreated, onConfigure }: {
  projectID: string
  onCreated: (draft: PlatformExperiment) => void
  onConfigure: (draft: ExperimentDraft) => void
}) {
  const [goal, setGoal] = useState('')
  const [touched, setTouched] = useState(false)
  const [sending, setSending] = useState(false)
  const [moving, setMoving] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [refused, setRefused] = useState<StatusReason | null>(null)
  const github = useGitHub()
  const app = useGitHubApp()
  const plainHost = app.error instanceof ApiError && app.error.reason === GITHUB_NOT_CONFIGURED
  // Read again while the page is open, so that planning is offered again once it can start.
  const hosting = useSWR(keys.hosting(), fetchHosting, { refreshInterval: POLL.standing, onSuccess: h => { if (h.available) setRefused(null) } })
  const unavailable = hosting.data && !hosting.data.available ? hosting.data.unavailable ?? null : refused
  const [repository, setRepository] = useState('')
  const box = useRef<HTMLTextAreaElement>(null)
  const placeholder = useTypedExamples(touched)

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
        placeholder={placeholder}
        // The page's styled scrollbars give an empty textarea a horizontal one; the sentence wraps instead.
        className="block max-h-[60vh] w-full resize-none overflow-y-auto overflow-x-hidden break-words px-6 pb-2 pt-5 text-lg"
        value={goal}
        onFocus={() => setTouched(true)}
        onChange={e => { setTouched(true); setGoal(e.target.value) }}
        onKeyDown={e => {
          if (e.key !== 'Enter' || e.shiftKey || e.nativeEvent.isComposing) return
          e.preventDefault()
          void plan()
        }}
      />
      <BriefGuide className="px-6 pb-3" />
      {unavailable && (
        <div className="px-4 pb-3">
          <Callout tone="warning" className="flex flex-wrap items-center justify-between gap-2">
            <Text>{unavailable.summary}</Text>
            <Button size="sm" loading={moving} onClick={() => void configure()}>Set it up myself</Button>
            {unavailable.operator_detail && <Text variant="note" className="basis-full">Operator: {unavailable.operator_detail}</Text>}
          </Callout>
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
        {/* A disabled button shows no tooltip in every browser, so the reason sits on its wrapper. */}
        <span className="ml-auto" title={unavailable ? CANNOT_START : github === 'unconnected' ? CONNECT_FIRST : undefined}>
          <Button variant="primary" loading={sending} disabled={!ready && !sending} onClick={plan} aria-keyshortcuts="Enter">
            Plan it
          </Button>
        </span>
      </Inline>
    </div>
  )
}

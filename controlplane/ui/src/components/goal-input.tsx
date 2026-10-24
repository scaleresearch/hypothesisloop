'use client'

import { useEffect, useLayoutEffect, useRef, useState } from 'react'
import useSWR from 'swr'
import { Field, Label } from '@headlessui/react'
import { GITHUB_NOT_CONFIGURED } from '@/lib/api/github'
import { fetchAgentSandbox } from '@/lib/api/agentsandbox'
import { createDraft } from '@/lib/api/planning'
import { ApiError } from '@/lib/api/request'
import { formDraftFromGoal, agentSandboxRefusal, MAX_GOAL_LENGTH } from '@/lib/planning'
import { useGitHubApp } from '@/lib/github-connect'
import { GOAL_EXAMPLES } from '@/lib/goal-examples'
import { GoalGuide } from '@/components/goal-guide'
import { InstallGitHub, useGitHub } from '@/components/connect-github-step'
import { codeFields, DEFAULT_BRANCH, namesRepository, RepositorySelect } from '@/components/repository-select'
import { Button } from '@/components/ui/button'
import { Input, Textarea } from '@/components/ui/input'
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
  const agentSandbox = useSWR(keys.agentSandbox(), fetchAgentSandbox, { refreshInterval: POLL.standing, onSuccess: h => { if (h.available) setRefused(null) } })
  const unavailable = agentSandbox.data && !agentSandbox.data.available ? agentSandbox.data.unavailable ?? null : refused
  const [repository, setRepository] = useState('')
  const [branch, setBranch] = useState(DEFAULT_BRANCH)
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
      onCreated(await createDraft(projectID, goal.trim(), codeFields(repository, branch)))
    } catch (e) {
      const agentSandboxReason = agentSandboxRefusal(e)
      if (agentSandboxReason) {
        setRefused(agentSandboxReason)
        void agentSandbox.mutate()
      } else setError(errorMessage(e))
      setSending(false)
    }
  }

  async function configure() {
    setMoving(true)
    setError(null)
    try {
      onConfigure(await formDraftFromGoal(projectID, goal.trim(), repository, branch))
    } catch (e) {
      setError(`Could not save your text as a draft: ${errorMessage(e)}`)
    }
    setMoving(false)
  }

  return (
    <div className="grid gap-2 px-4 py-3">
      <div className="flex items-start gap-3">
        <Textarea
          ref={box}
          bare
          rows={1}
          maxLength={MAX_GOAL_LENGTH}
          aria-label={ASK}
          placeholder={placeholder}
          // The page's styled scrollbars give an empty textarea a horizontal one; the sentence wraps instead.
          className="block max-h-[40vh] min-w-0 flex-1 resize-none overflow-y-auto overflow-x-hidden break-words px-2 py-1.5 text-base"
          value={goal}
          onFocus={() => setTouched(true)}
          onChange={e => { setTouched(true); setGoal(e.target.value) }}
          onKeyDown={e => {
            if (e.key !== 'Enter' || e.shiftKey || e.nativeEvent.isComposing) return
            e.preventDefault()
            void plan()
          }}
        />
        {/* A disabled button shows no tooltip in every browser, so the reason sits on its wrapper. */}
        <span title={unavailable ? CANNOT_START : github === 'unconnected' ? CONNECT_FIRST : undefined}>
          <Button variant="primary" loading={sending} disabled={!ready && !sending} onClick={plan} aria-keyshortcuts="Enter">
            Plan it
          </Button>
        </span>
      </div>
      {unavailable && (
        <Callout tone="warning" className="flex flex-wrap items-center justify-between gap-2">
          <Text>{unavailable.summary}</Text>
          <Button size="sm" loading={moving} onClick={() => void configure()}>Set it up myself</Button>
        </Callout>
      )}
      {agentSandbox.error && <InlineError className="block px-2">Could not check whether planning is available: {agentSandbox.error.message}</InlineError>}
      {error && <InlineError className="block px-2">{error}</InlineError>}
      <div className="flex flex-wrap items-start justify-between gap-x-4 gap-y-2 px-2">
        <GoalGuide className="min-w-0 flex-1" />
        {github === 'unconnected' && <InstallGitHub />}
        {(github === 'connected' || plainHost) && (
          <Inline gap="md">
            <Field className="flex min-w-0 items-center gap-2">
              <Label className="text-xs text-muted">Repository</Label>
              <RepositorySelect compact value={repository} onChange={setRepository} />
            </Field>
            {namesRepository(repository) && (
              <Field className="flex items-center gap-2">
                <Label className="text-xs text-muted">Branch</Label>
                <Input mono className="w-32" value={branch} onChange={e => setBranch(e.target.value)} />
              </Field>
            )}
          </Inline>
        )}
      </div>
    </div>
  )
}

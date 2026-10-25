'use client'

import { clsx } from 'clsx'
import { ChevronDown, FolderGit2 } from 'lucide-react'
import { useEffect, useLayoutEffect, useRef, useState } from 'react'
import type { ReactNode } from 'react'
import useSWR from 'swr'
import { fetchAgentSandbox } from '@/lib/api/agentsandbox'
import { createDraft } from '@/lib/api/planning'
import { ApiError } from '@/lib/api/request'
import { formDraftFromGoal, agentSandboxRefusal, MAX_GOAL_LENGTH } from '@/lib/planning'
import { installationSettingsURL } from '@/lib/github-connect'
import { useTenant } from '@/lib/tenant'
import { GOAL_EXAMPLES } from '@/lib/goal-examples'
import { ASK_OWNER, ConnectGitHubDialog, useGitHub, useGitHubAccounts, usePlainGitHost } from '@/components/connect-github-step'
import { codeFields, DEFAULT_BRANCH, namesRepository, RepositorySelect } from '@/components/repository-select'
import { NO_GOAL, NO_REPOSITORY } from '@/components/platform-experiment-form/model'
import { Button } from '@/components/ui/button'
import { Dialog } from '@/components/ui/dialog'
import { Input, Textarea } from '@/components/ui/input'
import { inlineLinkClass } from '@/components/ui/link'
import { Callout, InlineError } from '@/components/ui/status-message'
import { Text } from '@/components/ui/text'
import { errorMessage } from '@/lib/errors'
import { keys } from '@/lib/keys'
import { POLL } from '@/lib/poll'
import type { ExperimentDraft, PlatformExperiment, StatusReason } from '@/types/api'

const ASK = 'Describe the goal of your experiment'

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

// The refusals about GitHub a planned experiment can meet, as what the box says and its one fix.
type GitHubFix = 'connect' | 'review' | 'choose'
const GITHUB_REFUSALS: Record<string, { text: string; fix: GitHubFix }> = {
  installation_not_connected: { text: 'GitHub is not connected.', fix: 'connect' },
  git_host_not_connected: { text: 'GitHub is not connected.', fix: 'connect' },
  installation_gone: { text: 'GitHub is no longer connected.', fix: 'connect' },
  installation_suspended: { text: 'GitHub access is suspended.', fix: 'review' },
  git_host_permission_missing: { text: 'GitHub has not granted a permission we need.', fix: 'review' },
  repository_creation_not_permitted: { text: 'GitHub does not let us create a repository there.', fix: 'review' },
  repository_account_required: { text: 'Several GitHub accounts are connected. Choose where the repository goes.', fix: 'choose' },
}

// What the control shows for the value RepositorySelect holds, as GitHub writes it.
function repositoryLabel(value: string, defaultAccount?: string): string {
  if (namesRepository(value)) return value
  const account = value === '' ? defaultAccount : value.slice(1)
  return account ? `${account} / new repository` : 'New repository'
}

/**
 * The shortest way to an experiment: a few sentences saying what to find out. Plan creates a draft
 * with a new repository, or an existing one chosen beside it, and opens it. A missing goal or
 * GitHub connection is named where it belongs, and a failed plan leads to the full form.
 */
export function GoalInput({ projectID, large = false, onCreated, onConfigure }: {
  projectID: string
  /** The whole page is the box, as nothing else is on it yet. */
  large?: boolean
  onCreated: (draft: PlatformExperiment) => void
  onConfigure: (draft: ExperimentDraft) => void
}) {
  const { org } = useTenant()
  const owner = org.actions.includes('edit')
  const [goal, setGoal] = useState('')
  const [touched, setTouched] = useState(false)
  const [sending, setSending] = useState(false)
  const [moving, setMoving] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [githubRefusal, setGitHubRefusal] = useState<string | null>(null)
  const [refused, setRefused] = useState<StatusReason | null>(null)
  const [goalMissing, setGoalMissing] = useState(false)
  const [repositoryMissing, setRepositoryMissing] = useState(false)
  const [connecting, setConnecting] = useState(false)
  const github = useGitHub()
  const accounts = useGitHubAccounts()
  const defaultAccount = (accounts?.find(a => a.default) ?? accounts?.[0])?.account_login
  const plainHost = usePlainGitHost()
  // Read again while the page is open, so that planning is offered again once it can start.
  const agentSandbox = useSWR(keys.agentSandbox(), fetchAgentSandbox, { refreshInterval: POLL.standing, onSuccess: h => { if (h.available) setRefused(null) } })
  const unavailable = agentSandbox.data && !agentSandbox.data.available ? agentSandbox.data.unavailable ?? null : refused
  const [repository, setRepository] = useState('')
  const [choosing, setChoosing] = useState(false)
  const box = useRef<HTMLTextAreaElement>(null)
  const repositoryField = useRef<HTMLInputElement>(null)
  const callout = useRef<HTMLDivElement>(null)
  const placeholder = useTypedExamples(touched)

  useLayoutEffect(() => {
    const el = box.current
    if (!el || el.scrollHeight === 0) return
    el.style.height = 'auto'
    el.style.height = `${el.scrollHeight}px`
  }, [goal, large])

  // One rule for the button and for Enter: say what is missing, beside what it concerns.
  async function plan() {
    if (sending) return
    const noGoal = goal.trim() === ''
    const noRepository = plainHost && repository.trim() === ''
    setGoalMissing(noGoal)
    setRepositoryMissing(noRepository)
    if (noGoal) return box.current?.focus()
    if (noRepository) return repositoryField.current?.focus()
    if (!plainHost && github === 'unconnected') return setConnecting(true)
    if (unavailable) {
      callout.current?.scrollIntoView({ block: 'nearest', behavior: 'smooth' })
      callout.current?.querySelector('button')?.focus()
      return
    }
    setSending(true)
    setError(null)
    setGitHubRefusal(null)
    try {
      // With no repository chosen the platform creates a private one from the template.
      onCreated(await createDraft(projectID, goal.trim(), plainHost ? { repository: repository.trim(), branch: DEFAULT_BRANCH } : codeFields(repository, DEFAULT_BRANCH)))
    } catch (e) {
      const agentSandboxReason = agentSandboxRefusal(e)
      if (agentSandboxReason) {
        setRefused(agentSandboxReason)
        void agentSandbox.mutate()
      } else if (e instanceof ApiError && e.reason && e.reason in GITHUB_REFUSALS) setGitHubRefusal(e.reason)
      else setError(errorMessage(e))
      setSending(false)
    }
  }

  async function configure() {
    setMoving(true)
    setError(null)
    try {
      onConfigure(await formDraftFromGoal(projectID, goal.trim(), plainHost ? repository.trim() : repository, DEFAULT_BRANCH))
    } catch (e) {
      setError(`Could not save your text as a draft: ${errorMessage(e)}`)
    }
    setMoving(false)
  }

  const chooser = !plainHost && github === 'connected'
  const installation = accounts?.find(a => a.default) ?? accounts?.[0]
  let fix: ReactNode = null
  const refusal = githubRefusal ? GITHUB_REFUSALS[githubRefusal] : null
  if (refusal?.fix === 'connect') {
    fix = owner ? <Button type="button" variant="link" size="sm" onClick={() => setConnecting(true)}>Connect GitHub</Button> : <Text as="span" variant="note">{ASK_OWNER}</Text>
  } else if (refusal?.fix === 'review' && installation) {
    fix = <a href={installationSettingsURL(installation)} target="_blank" rel="noreferrer" className={clsx(inlineLinkClass, 'text-xs')}>Review on GitHub</a>
  } else if (refusal?.fix === 'choose') {
    fix = <Button type="button" variant="link" size="sm" onClick={() => setChoosing(true)}>Choose</Button>
  }

  return (
    <div>
      <Textarea
        ref={box}
        bare
        rows={large ? 8 : 3}
        maxLength={MAX_GOAL_LENGTH}
        aria-label={ASK}
        invalid={goalMissing}
        aria-describedby={goalMissing ? 'goal-missing' : undefined}
        placeholder={placeholder}
        // The page's styled scrollbars give an empty textarea a horizontal one; the sentence wraps instead.
        className={clsx('block max-h-[60vh] w-full resize-none overflow-y-auto overflow-x-hidden break-words px-6 pb-3 pt-5', large ? 'text-xl' : 'text-lg')}
        value={goal}
        onFocus={() => setTouched(true)}
        onChange={e => {
          setTouched(true)
          setGoalMissing(false)
          setGoal(e.target.value)
        }}
        onKeyDown={e => {
          if (e.key !== 'Enter' || e.shiftKey || e.nativeEvent.isComposing) return
          e.preventDefault()
          void plan()
        }}
      />
      {goalMissing && <InlineError className="block px-6 pb-3 text-sm"><span id="goal-missing">{NO_GOAL}</span></InlineError>}
      {unavailable && (
        <div ref={callout} className="scroll-my-4 px-4 pb-3">
          <Callout tone="warning" className="flex flex-wrap items-center justify-between gap-2">
            <Text>{unavailable.summary}</Text>
            <Button size="sm" loading={moving} onClick={() => void configure()}>Set up manually</Button>
          </Callout>
        </div>
      )}
      {agentSandbox.error && <InlineError className="block px-6 pb-3">Could not check whether planning is available: {agentSandbox.error.message}</InlineError>}
      {error && <InlineError className="block px-6 pb-3">{error}</InlineError>}
      {refusal && (
        <div className="flex flex-wrap items-center gap-x-3 gap-y-1 px-6 pb-3">
          <InlineError className="text-sm">{refusal.text}</InlineError>
          {fix}
        </div>
      )}
      <div className="flex flex-wrap items-start gap-x-4 gap-y-2 border-t px-6 py-3">
        {plainHost && (
          <div className="grid gap-1">
            <Input
              ref={repositoryField}
              className="w-64 max-w-full"
              aria-label="Repository"
              placeholder="Repository (owner/name)"
              invalid={repositoryMissing}
              aria-describedby={repositoryMissing ? 'repository-missing' : undefined}
              value={repository}
              onChange={e => { setRepositoryMissing(false); setRepository(e.target.value) }}
            />
            {repositoryMissing && <InlineError><span id="repository-missing">{NO_REPOSITORY}</span></InlineError>}
          </div>
        )}
        {chooser && (
          <Button variant="ghost" className="ml-auto min-w-0 max-w-full" aria-label={`Repository: ${repositoryLabel(repository, defaultAccount)}`} onClick={() => setChoosing(true)}>
            <FolderGit2 aria-hidden className="size-4 shrink-0" />
            <span className="truncate">{repositoryLabel(repository, defaultAccount)}</span>
            <ChevronDown aria-hidden className="size-4 shrink-0" />
          </Button>
        )}
        <Button variant="primary" className={clsx(!chooser && 'ml-auto')} loading={sending} onClick={() => void plan()} aria-keyshortcuts="Enter">
          Plan experiment
        </Button>
      </div>
      <Dialog open={choosing} onClose={() => setChoosing(false)} title="Repository" size="sm">
        <RepositorySelect value={repository} onChange={r => { setRepository(r); setGitHubRefusal(null); setChoosing(false) }} />
      </Dialog>
      <ConnectGitHubDialog
        open={connecting}
        onClose={() => setConnecting(false)}
        onConnected={() => {
          setConnecting(false)
          setGitHubRefusal(null)
          void plan()
        }}
      />
    </div>
  )
}

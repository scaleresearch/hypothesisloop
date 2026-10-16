'use client'

import { useEffect, useRef, useState } from 'react'
import { Field, Label } from '@headlessui/react'
import { githubCardPath } from '@/lib/github-connect'
import { useTenant } from '@/lib/tenant'
import { GOAL_EXAMPLES } from '@/lib/goal-examples'
import { createDraft, MAX_GOAL_LENGTH } from '@/lib/planning'
import type { Draft } from '@/lib/planning'
import { Button } from '@/components/ui/button'
import { Textarea } from '@/components/ui/input'
import { TextLink } from '@/components/ui/link'
import { Inline } from '@/components/ui/stack'
import { InlineError } from '@/components/ui/status-message'
import { Text } from '@/components/ui/text'
import { useGitHub } from '@/components/connect-github-step'
import { RepositorySelect, repositoryFields } from '@/components/repository-select'
import { BriefPoints } from '@/components/brief-points'

const CONNECT_FIRST = 'Connect GitHub in Settings to start'
const ASK = 'What do you want to find out?'

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
 * The shortest way to an experiment: one sentence saying what to find out. The platform plans
 * the rest and proposes it on the draft's page, where the person approves it. Once the box is
 * taken it offers the points the planner cannot read from the code, as lines to add.
 */
export function GoalInput({ projectID, onCreated }: { projectID: string; onCreated: (draft: Draft) => void }) {
  const { org } = useTenant()
  const [goal, setGoal] = useState('')
  const [touched, setTouched] = useState(false)
  const [sending, setSending] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const github = useGitHub()
  const [repository, setRepository] = useState('')
  const placeholder = useTypedExamples(touched)
  const box = useRef<HTMLTextAreaElement>(null)

  // One rule for the button and for Enter: a sentence, GitHub connected where it is needed, and no plan already on its way.
  const ready = goal.trim() !== '' && !sending && github !== 'unconnected'

  async function plan() {
    if (!ready) return
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
    <div>
      <Textarea
        ref={box}
        bare
        rows={Math.min(10, Math.max(3, goal.split('\n').length + 1))}
        maxLength={MAX_GOAL_LENGTH}
        aria-label={ASK}
        placeholder={placeholder}
        // The page's styled scrollbars give an empty textarea a horizontal one; the sentence wraps instead.
        className="block w-full resize-none overflow-x-hidden break-words px-6 pb-2 pt-5 text-lg"
        value={goal}
        onFocus={() => setTouched(true)}
        onChange={e => { setTouched(true); setGoal(e.target.value) }}
        onKeyDown={e => {
          if (e.key !== 'Enter' || e.shiftKey || e.nativeEvent.isComposing) return
          e.preventDefault()
          void plan()
        }}
      />
      {touched && <BriefPoints text={goal} onChange={setGoal} box={box} className="px-6 pb-3" />}
      <Inline gap="md" className="px-4 pb-4">
        {github === 'connected' && (
          <Field>
            <Label className="sr-only">Repository</Label>
            <RepositorySelect compact value={repository} onChange={setRepository} />
          </Field>
        )}
        {github === 'unconnected' && (
          <Text variant="note" className="px-2">
            <TextLink inline href={githubCardPath(org.id)}>Connect GitHub in Settings</TextLink> to start.
          </Text>
        )}
        {error && <InlineError className="px-2">{error}</InlineError>}
        {/* A disabled button shows no tooltip in every browser, so the reason sits on its wrapper. */}
        <span className="ml-auto" title={github === 'unconnected' ? CONNECT_FIRST : undefined}>
          <Button variant="primary" loading={sending} disabled={goal.trim() === '' || github === 'unconnected'} onClick={plan} aria-keyshortcuts="Enter">
            Plan it
          </Button>
        </span>
      </Inline>
    </div>
  )
}

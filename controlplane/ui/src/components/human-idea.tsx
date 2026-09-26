'use client'

import { useState } from 'react'
import { registerHumanHypothesis, addHumanHypothesisComment, addHumanHypothesisLink } from '@/lib/api'
import type { HypothesisSearchHit, SimilarHypothesis } from '@/lib/api'
import type { HypothesisLinkType, HypothesisWithJobs } from '@/types'
import { Badge } from '@/components/ui/badge'
import { Button, Chip } from '@/components/ui/button'
import { Textarea } from '@/components/ui/input'
import { InlineError } from '@/components/ui/status-message'
import { Eyebrow } from '@/components/ui/text'
import { HypothesisPicker } from '@/components/hypothesis-search'
import { useReadOnly } from '@/lib/read-only'

/** Marks a hypothesis or note that came from a person, wherever one is shown. */
function HumanAuthor({ author }: { author: string }) {
  return (
    <span className="inline-flex items-center gap-1.5">
      <Badge status="human">human</Badge>
      <span>{author}</span>
    </span>
  )
}

/** The author of a hypothesis or note: an agent id, or a person marked as such. */
export function PoolAuthor({ source, agentID, author }: { source?: string; agentID: string; author?: string }) {
  if (source === 'human') return <HumanAuthor author={author ?? ''} />
  return <span className="font-mono text-xs">{agentID}</span>
}

interface IdeaParent {
  /** A hypothesis id, or the platform_experiment_id for the objective root. */
  id: string
  label: string
}

function TargetList({ label, targets, onRemove }: {
  label: string
  targets: HypothesisSearchHit[]
  onRemove: (id: string) => void
}) {
  if (targets.length === 0) return null
  return (
    <div className="grid grid-cols-[minmax(0,1fr)] gap-1">
      <Eyebrow>{label}</Eyebrow>
      {targets.map(t => (
        <div key={t.id} className="flex items-center gap-2 text-xs">
          <span className="min-w-0 flex-1 truncate" title={t.text}>{t.text}</span>
          <Button variant="link" size="sm" onClick={() => onRemove(t.id)} aria-label={`Remove ${t.text}`}>Remove</Button>
        </div>
      ))}
    </div>
  )
}

// Anyone watching a run can add an idea to the graph from here, placed under the hypothesis (or
// objective) they selected. Before this, the only human steering channel was rewriting the
// platform experiment's description, which rewrites the brief for every agent at once.
export function AddIdeaForm({ platformExperimentID, parent, onAdded }: {
  platformExperimentID: string
  parent: IdeaParent
  onAdded: (id: string) => void
}) {
  const [text, setText] = useState('')
  const [crossType, setCrossType] = useState<'contradicts' | 'related'>('contradicts')
  const [contradicts, setContradicts] = useState<HypothesisSearchHit[]>([])
  const [related, setRelated] = useState<HypothesisSearchHit[]>([])
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const [existingID, setExistingID] = useState('')
  const [similar, setSimilar] = useState<SimilarHypothesis[]>([])
  const readOnly = useReadOnly()

  const picked = [...contradicts, ...related].map(t => t.id)

  function pick(hit: HypothesisSearchHit) {
    if (crossType === 'contradicts') setContradicts([...contradicts, hit])
    else setRelated([...related, hit])
  }

  async function submit(distinctFrom: string[] = []) {
    setBusy(true)
    setError('')
    setExistingID('')
    setSimilar([])
    try {
      const h = await registerHumanHypothesis(platformExperimentID, text.trim(), [
        { type: 'builds_on', to: parent.id },
        ...contradicts.map(t => ({ type: 'contradicts' as const, to: t.id })),
        ...related.map(t => ({ type: 'related' as const, to: t.id })),
      ], distinctFrom)
      if ('similar' in h) setSimilar(h.similar)
      else if (h.already_existed) setExistingID(h.id)
      else onAdded(h.id)
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
    } finally {
      setBusy(false)
    }
  }

  if (readOnly) return null
  return (
    <div className="grid grid-cols-[minmax(0,1fr)] max-w-[760px] gap-2.5">
      <p className="text-xs text-muted">
        Your idea joins the graph the agents read and write, under the same duplicate check.
        No job runs against it: an agent that wants to test it registers its own hypothesis
        building on yours. It is registered under the name you signed in with.
      </p>
      <div className="text-xs">
        <Eyebrow>Builds on</Eyebrow>{' '}
        <span title={parent.label}>{parent.label}</span>
      </div>
      <Textarea
        className="min-h-[70px] w-full"
        value={text}
        onChange={e => setText(e.target.value)}
        placeholder="e.g. the retry loop is masking a bad learning-rate schedule; try a linear warmup instead"
      />
      <details>
        <summary className="cursor-pointer text-xs text-accent hover:underline">
          Mark hypotheses it contradicts or relates to (optional)
        </summary>
        <div className="grid grid-cols-[minmax(0,1fr)] mt-2 gap-2">
          <div className="flex gap-1.5">
            <Chip active={crossType === 'contradicts'} onClick={() => setCrossType('contradicts')}>Contradicts</Chip>
            <Chip active={crossType === 'related'} onClick={() => setCrossType('related')}>Related</Chip>
          </div>
          <HypothesisPicker platformExperimentID={platformExperimentID} exclude={[parent.id, ...picked]} onPick={pick} />
          <TargetList label="Contradicts" targets={contradicts} onRemove={id => setContradicts(contradicts.filter(t => t.id !== id))} />
          <TargetList label="Related" targets={related} onRemove={id => setRelated(related.filter(t => t.id !== id))} />
        </div>
      </details>
      <div className="flex items-center gap-2.5">
        <Button size="sm" variant="primary" disabled={busy || text.trim() === ''} onClick={() => submit()}>
          {busy ? 'Adding…' : 'Add idea'}
        </Button>
        {existingID && (
          <span className="text-xs text-muted">
            An equivalent idea already exists, so nothing was added and your links were not applied.{' '}
            <Button variant="link" size="sm" onClick={() => onAdded(existingID)}>Show it</Button>
          </span>
        )}
        {error && <InlineError>{error}</InlineError>}
      </div>
      {similar.length > 0 && (
        <div className="grid grid-cols-[minmax(0,1fr)] gap-1 text-xs">
          <span className="text-muted">
            Nothing was added yet: these read close to your idea, or it builds on a refuted one.
            If one is your idea, open it instead; mark one your idea opposes as Contradicts.
          </span>
          {similar.map(h => (
            <div key={h.id} className="flex items-center gap-2">
              <Badge status={h.status}>{h.status}</Badge>
              <span className="min-w-0 flex-1 truncate" title={h.text}>{h.text}</span>
              <Button variant="link" size="sm" onClick={() => onAdded(h.id)}>Show it</Button>
            </div>
          ))}
          <div>
            <Button size="sm" disabled={busy} onClick={() => submit(similar.map(h => h.id))}>
              None of these is my idea, add it
            </Button>
          </div>
        </div>
      )}
    </div>
  )
}

const LINK_TYPE_LABELS: Record<HypothesisLinkType, string> = {
  builds_on: 'Builds on',
  contradicts: 'Contradicts',
  related: 'Related',
}

// The registering agent alone may link from its hypothesis; a human-authored one is anyone's.
export function AddLinkForm({ hypothesis, onAdded }: { hypothesis: HypothesisWithJobs; onAdded: () => void }) {
  const [type, setType] = useState<HypothesisLinkType>('related')
  const [target, setTarget] = useState<HypothesisSearchHit | null>(null)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const readOnly = useReadOnly()

  if (readOnly) return null
  if (hypothesis.source !== 'human') {
    return (
      <p className="mt-3 text-xs text-subtle">
        Only <span className="font-mono">{hypothesis.agent_id}</span> can add links from this hypothesis.
        To connect your own idea, add it from the graph, building on this one.
      </p>
    )
  }

  async function submit(to: string) {
    setBusy(true)
    setError('')
    try {
      await addHumanHypothesisLink(hypothesis.id, type, to)
      setTarget(null)
      onAdded()
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="grid grid-cols-[minmax(0,1fr)] mt-3 max-w-[760px] gap-2">
      <Eyebrow>Add link</Eyebrow>
      <div className="flex gap-1.5">
        {(Object.keys(LINK_TYPE_LABELS) as HypothesisLinkType[]).map(t => (
          <Chip key={t} active={type === t} onClick={() => setType(t)}>{LINK_TYPE_LABELS[t]}</Chip>
        ))}
      </div>
      {target ? (
        <div className="flex items-center gap-2.5 text-xs">
          <span className="min-w-0 flex-1 truncate" title={target.text}>{target.text}</span>
          <Button size="sm" variant="primary" disabled={busy} onClick={() => submit(target.id)}>
            {busy ? 'Linking…' : `Add ${LINK_TYPE_LABELS[type].toLowerCase()} link`}
          </Button>
          <Button variant="link" size="sm" onClick={() => setTarget(null)}>Change</Button>
        </div>
      ) : (
        <HypothesisPicker platformExperimentID={hypothesis.platform_experiment_id} exclude={[hypothesis.id]} onPick={setTarget} />
      )}
      {error && <InlineError>{error}</InlineError>}
    </div>
  )
}

/** A note against one hypothesis, under the name the writer signed in with. */
export function AddCommentForm({ hypothesisID, onAdded }: { hypothesisID: string; onAdded: () => void }) {
  const [text, setText] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const readOnly = useReadOnly()

  async function submit() {
    setBusy(true)
    setError('')
    try {
      await addHumanHypothesisComment(hypothesisID, text.trim())
      setText('')
      onAdded()
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
    } finally {
      setBusy(false)
    }
  }

  if (readOnly) return null
  return (
    <div className="mt-3 grid max-w-[760px] gap-2">
      <Textarea
        className="min-h-[60px] w-full"
        value={text}
        onChange={e => setText(e.target.value)}
        placeholder="A note on this claim: amend it, cross-reference a finding, say why you'd drop it."
      />
      <div className="flex items-center gap-2.5">
        <Button size="sm" disabled={busy || text.trim() === ''} onClick={submit}>
          {busy ? 'Adding…' : 'Add comment'}
        </Button>
        {error && <InlineError>{error}</InlineError>}
      </div>
    </div>
  )
}

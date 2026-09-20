'use client'

import { useState } from 'react'
import { registerHumanHypothesis, addHumanHypothesisComment, addHumanHypothesisLink } from '@/lib/api'
import type { HypothesisSearchHit, SimilarHypothesis } from '@/lib/api'
import type { HypothesisLinkType, HypothesisWithJobs } from '@/types'
import { Badge } from '@/components/ui/badge'
import { Button, Chip } from '@/components/ui/button'
import { HypothesisPicker, INPUT_STYLE } from '@/components/hypothesis-search'

/** Marks a hypothesis or note that came from a person, wherever one is shown. */
function HumanAuthor({ author }: { author: string }) {
  return (
    <span style={{ display: 'inline-flex', alignItems: 'center', gap: 6 }}>
      <span className="badge badge-human">human</span>
      <span>{author}</span>
    </span>
  )
}

/** The author of a hypothesis or note: an agent id, or a person marked as such. */
export function PoolAuthor({ source, agentID, author }: { source?: string; agentID: string; author?: string }) {
  if (source === 'human') return <HumanAuthor author={author ?? ''} />
  return <span className="mono">{agentID}</span>
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
    <div className="hg-stack" style={{ gap: 4 }}>
      <span className="uppercase-label">{label}</span>
      {targets.map(t => (
        <div key={t.id} style={{ display: 'flex', alignItems: 'center', gap: 8, fontSize: 12 }}>
          <span className="hg-outline-text" title={t.text}>{t.text}</span>
          <Button variant="link" onClick={() => onRemove(t.id)} aria-label={`Remove ${t.text}`}>Remove</Button>
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

  return (
    <div className="hg-stack" style={{ gap: 10, maxWidth: 760 }}>
      <p className="text-muted" style={{ fontSize: 12 }}>
        Your idea joins the graph the agents read and write, under the same duplicate check.
        No job runs against it: an agent that wants to test it registers its own hypothesis
        building on yours. It is registered under the name you signed in with.
      </p>
      <div style={{ fontSize: 12 }}>
        <span className="uppercase-label">Builds on</span>{' '}
        <span title={parent.label}>{parent.label}</span>
      </div>
      <textarea
        style={{ ...INPUT_STYLE, minHeight: 70, resize: 'vertical' }}
        value={text}
        onChange={e => setText(e.target.value)}
        placeholder="e.g. the retry loop is masking a bad learning-rate schedule; try a linear warmup instead"
      />
      <details>
        <summary className="text-link" style={{ fontSize: 12, cursor: 'pointer' }}>
          Mark hypotheses it contradicts or relates to (optional)
        </summary>
        <div className="hg-stack" style={{ marginTop: 8 }}>
          <div style={{ display: 'flex', gap: 6 }}>
            <Chip active={crossType === 'contradicts'} onClick={() => setCrossType('contradicts')}>Contradicts</Chip>
            <Chip active={crossType === 'related'} onClick={() => setCrossType('related')}>Related</Chip>
          </div>
          <HypothesisPicker platformExperimentID={platformExperimentID} exclude={[parent.id, ...picked]} onPick={pick} />
          <TargetList label="Contradicts" targets={contradicts} onRemove={id => setContradicts(contradicts.filter(t => t.id !== id))} />
          <TargetList label="Related" targets={related} onRemove={id => setRelated(related.filter(t => t.id !== id))} />
        </div>
      </details>
      <div style={{ display: 'flex', alignItems: 'center', gap: 10 }}>
        <Button size="sm" variant="primary" disabled={busy || text.trim() === ''} onClick={() => submit()}>
          {busy ? 'Adding…' : 'Add idea'}
        </Button>
        {existingID && (
          <span className="text-muted" style={{ fontSize: 12 }}>
            An equivalent idea already exists, so nothing was added and your links were not applied.{' '}
            <Button variant="link" onClick={() => onAdded(existingID)}>Show it</Button>
          </span>
        )}
        {error && <span className="text-error" style={{ fontSize: 12 }}>{error}</span>}
      </div>
      {similar.length > 0 && (
        <div className="hg-stack" style={{ gap: 4, fontSize: 12 }}>
          <span className="text-muted">
            Nothing was added yet: these read close to your idea, or it builds on a refuted one.
            If one is your idea, open it instead; mark one your idea opposes as Contradicts.
          </span>
          {similar.map(h => (
            <div key={h.id} style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
              <Badge status={h.status}>{h.status}</Badge>
              <span className="hg-outline-text" title={h.text}>{h.text}</span>
              <Button variant="link" onClick={() => onAdded(h.id)}>Show it</Button>
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

  if (hypothesis.source !== 'human') {
    return (
      <p className="text-dim" style={{ marginTop: 12 }}>
        Only <span className="mono">{hypothesis.agent_id}</span> can add links from this hypothesis.
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
    <div className="hg-stack" style={{ maxWidth: 760, marginTop: 12 }}>
      <span className="uppercase-label">Add link</span>
      <div style={{ display: 'flex', gap: 6 }}>
        {(Object.keys(LINK_TYPE_LABELS) as HypothesisLinkType[]).map(t => (
          <Chip key={t} active={type === t} onClick={() => setType(t)}>{LINK_TYPE_LABELS[t]}</Chip>
        ))}
      </div>
      {target ? (
        <div style={{ display: 'flex', alignItems: 'center', gap: 10, fontSize: 12 }}>
          <span className="hg-outline-text" title={target.text}>{target.text}</span>
          <Button size="sm" variant="primary" disabled={busy} onClick={() => submit(target.id)}>
            {busy ? 'Linking…' : `Add ${LINK_TYPE_LABELS[type].toLowerCase()} link`}
          </Button>
          <Button variant="link" onClick={() => setTarget(null)}>Change</Button>
        </div>
      ) : (
        <HypothesisPicker platformExperimentID={hypothesis.platform_experiment_id} exclude={[hypothesis.id]} onPick={setTarget} />
      )}
      {error && <span className="text-error" style={{ fontSize: 12 }}>{error}</span>}
    </div>
  )
}

/** A note against one hypothesis, under the name the writer signed in with. */
export function AddCommentForm({ hypothesisID, onAdded }: { hypothesisID: string; onAdded: () => void }) {
  const [text, setText] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')

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

  return (
    <div style={{ display: 'grid', gap: 8, maxWidth: 760, marginTop: 12 }}>
      <textarea
        style={{ ...INPUT_STYLE, minHeight: 60, resize: 'vertical' }}
        value={text}
        onChange={e => setText(e.target.value)}
        placeholder="A note on this claim: amend it, cross-reference a finding, say why you'd drop it."
      />
      <div style={{ display: 'flex', alignItems: 'center', gap: 10 }}>
        <Button size="sm" disabled={busy || text.trim() === ''} onClick={submit}>
          {busy ? 'Adding…' : 'Add comment'}
        </Button>
        {error && <span className="text-error" style={{ fontSize: 12 }}>{error}</span>}
      </div>
    </div>
  )
}

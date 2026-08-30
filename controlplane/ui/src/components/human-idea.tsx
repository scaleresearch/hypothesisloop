'use client'

import { useState } from 'react'
import type { CSSProperties } from 'react'
import { registerHumanHypothesis, addHumanHypothesisComment } from '@/lib/api'
import { Button } from '@/components/ui/button'

const INPUT_STYLE: CSSProperties = {
  width: '100%', padding: '7px 10px', border: '1px solid var(--border)', borderRadius: 6,
  fontSize: 13, fontFamily: 'inherit', boxSizing: 'border-box',
  background: 'var(--surface-2)', color: 'var(--foreground)',
}

/** Marks a row that came from a person rather than an agent, wherever a pool row is shown. */
export function HumanAuthor({ author }: { author: string }) {
  return (
    <span style={{ display: 'inline-flex', alignItems: 'center', gap: 6 }}>
      <span className="badge badge-human">human</span>
      <span>{author}</span>
    </span>
  )
}

/** The author of a pool row or note: an agent id, or a person's typed name marked as such. */
export function PoolAuthor({ source, agentID, author }: { source?: string; agentID: string; author?: string }) {
  if (source === 'human') return <HumanAuthor author={author ?? ''} />
  return <span className="mono">{agentID}</span>
}

// Anyone watching a run can add an idea to the pool from here. Before this, the only human
// steering channel was rewriting the platform experiment's description — which rewrites the brief
// for every agent at once, and has already failed to stop an in-flight retry loop.
export function AddIdeaForm({ platformExperimentID, onAdded }: { platformExperimentID: string; onAdded: () => void }) {
  const [text, setText] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const [result, setResult] = useState('')

  async function submit() {
    setBusy(true)
    setError('')
    setResult('')
    try {
      await registerHumanHypothesis(platformExperimentID, 'human', text.trim())
      setText('')
      setResult('Added to the pool. Agents read it alongside their own.')
      onAdded()
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
    } finally {
      setBusy(false)
    }
  }

  return (
    <div style={{ display: 'grid', gap: 8, maxWidth: 760 }}>
      <p className="text-muted" style={{ fontSize: 12 }}>
        Your idea joins the same pool the agents read and write, under the same duplicate check.
        It is an idea, not an instruction: no job runs against it, and an agent that wants to test
        it registers its own hypothesis naming yours. There is no login — it is registered under
        "human".
      </p>
      <textarea
        style={{ ...INPUT_STYLE, minHeight: 70, resize: 'vertical' }}
        value={text}
        onChange={e => setText(e.target.value)}
        placeholder="e.g. the retry loop is masking a bad learning-rate schedule — try a linear warmup instead"
      />
      <div style={{ display: 'flex', alignItems: 'center', gap: 10 }}>
        <Button size="sm" disabled={busy || text.trim() === ''} onClick={submit}>
          {busy ? 'Adding…' : 'Add idea'}
        </Button>
        {result && <span className="text-muted" style={{ fontSize: 12 }}>{result}</span>}
        {error && <span className="text-error" style={{ fontSize: 12 }}>{error}</span>}
      </div>
    </div>
  )
}

/** The same author field on a note against one hypothesis. */
export function AddCommentForm({ hypothesisID, onAdded }: { hypothesisID: string; onAdded: () => void }) {
  const [text, setText] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')

  async function submit() {
    setBusy(true)
    setError('')
    try {
      await addHumanHypothesisComment(hypothesisID, 'human', text.trim())
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
        placeholder="A note on this claim — amend it, cross-reference a finding, say why you'd drop it."
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

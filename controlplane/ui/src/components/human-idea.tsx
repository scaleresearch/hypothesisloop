'use client'

import { useState } from 'react'
import { addHumanHypothesisComment, addHumanHypothesisLink } from '@/lib/api'
import type { HypothesisSearchHit } from '@/lib/api'
import type { HypothesisLinkType, HypothesisWithJobs } from '@/types'
import { Button, Chip } from '@/components/ui/button'
import { Textarea } from '@/components/ui/input'
import { InlineError } from '@/components/ui/status-message'
import { Inline, Stack } from '@/components/ui/stack'
import { Eyebrow, Text } from '@/components/ui/text'
import { HypothesisPicker } from '@/components/hypothesis-search'
import { AgentLabel } from '@/components/agent-label'
import { useReadOnly } from '@/lib/read-only'

const LINK_TYPE_LABELS: Record<HypothesisLinkType, string> = {
  builds_on: 'Builds on',
  contradicts: 'Contradicts',
  related: 'Related',
}

// From an agent's hypothesis only that agent links; one no agent owns is anyone's.
export function AddLinkForm({ hypothesis, onAdded }: { hypothesis: HypothesisWithJobs; onAdded: () => void }) {
  const [type, setType] = useState<HypothesisLinkType>('related')
  const [target, setTarget] = useState<HypothesisSearchHit | null>(null)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const readOnly = useReadOnly()

  if (readOnly) return null
  if (hypothesis.agent_id) {
    return (
      <Text variant="hint" className="mt-3">
        Only <AgentLabel id={hypothesis.agent_id} /> can add links from this hypothesis.
        To connect your own idea, add it from the graph, building on this one.
      </Text>
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
    <Stack gap="sm" className="mt-3 max-w-3xl">
      <Eyebrow>Add link</Eyebrow>
      <Inline gap="xs">
        {(Object.keys(LINK_TYPE_LABELS) as HypothesisLinkType[]).map(t => (
          <Chip key={t} active={type === t} onClick={() => setType(t)}>{LINK_TYPE_LABELS[t]}</Chip>
        ))}
      </Inline>
      {target ? (
        <Inline nowrap className="text-xs">
          <span className="min-w-0 flex-1 truncate" title={target.text}>{target.text}</span>
          <Button size="sm" variant="primary" disabled={busy} onClick={() => submit(target.id)}>
            {busy ? 'Linking…' : `Add ${LINK_TYPE_LABELS[type].toLowerCase()} link`}
          </Button>
          <Button variant="link" size="sm" onClick={() => setTarget(null)}>Change</Button>
        </Inline>
      ) : (
        <HypothesisPicker platformExperimentID={hypothesis.platform_experiment_id} exclude={[hypothesis.id]} onPick={setTarget} />
      )}
      {error && <InlineError>{error}</InlineError>}
    </Stack>
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
    <Stack gap="sm" className="mt-3 max-w-3xl">
      <Textarea
        className="min-h-16 w-full"
        value={text}
        onChange={e => setText(e.target.value)}
        placeholder="A note on this claim: amend it, cross-reference a finding, say why you'd drop it."
      />
      <Inline>
        <Button size="sm" disabled={busy || text.trim() === ''} onClick={submit}>
          {busy ? 'Adding…' : 'Add comment'}
        </Button>
        {error && <InlineError>{error}</InlineError>}
      </Inline>
    </Stack>
  )
}

'use client'

import { useState } from 'react'
import { useSWRConfig } from 'swr'
import { formatDate, relTime } from '@/lib/format'
import { abandonDraft, reviseDraft } from '@/lib/planning'
import type { Draft } from '@/lib/planning'
import { useRoster } from '@/lib/roster'
import { useProjectRouter } from '@/lib/query-state'
import { actorLabel } from '@/components/actor'
import { AgentAtWork } from '@/components/agent-at-work'
import { ProposalForm } from '@/components/proposal-form'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardHeader } from '@/components/ui/card'
import { FormField } from '@/components/ui/form-field'
import { Textarea } from '@/components/ui/input'
import { PageHeader } from '@/components/ui/page-header'
import { Callout, InlineError } from '@/components/ui/status-message'

const said = (error: unknown) => (error instanceof Error ? error.message : String(error))

function Revise({ draft, declined, onDone }: { draft: Draft; declined: boolean; onDone: () => void }) {
  const [message, setMessage] = useState('')
  const [sending, setSending] = useState(false)
  const [error, setError] = useState<string | null>(null)

  async function revise() {
    setSending(true)
    setError(null)
    try {
      await reviseDraft(draft.id, message.trim())
      setMessage('')
      onDone()
    } catch (e) {
      setError(said(e))
    }
    setSending(false)
  }

  return (
    <Card>
      <CardHeader>{declined ? 'Answer the planner' : 'Ask for a change'}</CardHeader>
      <CardContent className="grid gap-2">
        <FormField
          label={declined ? 'What was missing?' : 'What should be different?'}
          hint={declined
            ? 'The planner starts again with what it knows and your answer.'
            : 'The planner starts again with what it knows and proposes anew. The proposal above stays until it does.'}
        >
          <Textarea
            rows={2}
            value={message}
            onChange={e => setMessage(e.target.value)}
            placeholder={declined ? 'e.g. The small transformer in train.py, judged by validation loss.' : 'e.g. It must fit a single RTX 4090. Measure latency too.'}
          />
        </FormField>
        <div className="flex items-center gap-3">
          <Button loading={sending} disabled={message.trim() === ''} onClick={revise}>Revise</Button>
          {error && <InlineError>{error}</InlineError>}
        </div>
      </CardContent>
    </Card>
  )
}

/** What the planner's machine is doing, while there is no proposal to read. */
function Planning({ draft, revising }: { draft: Draft; revising: boolean }) {
  const { data: agents } = useRoster(draft)
  const planner = agents?.find(a => a.flavor === 'planner')
  return (
    <Callout tone="info" className="grid gap-2">
      <p role="status" className="font-semibold">{revising ? 'The planner is changing its proposal.' : 'The planner is working out how to measure this.'}</p>
      <AgentAtWork agent={planner} />
      <p className="text-xs text-muted">
        {planner?.instance?.stopped && 'Abandon this draft and try once more. '}
        It has until {formatDate(draft.planning_ends_at)}; a draft with no proposal by then is closed. This page updates by itself.
      </p>
    </Callout>
  )
}

/**
 * The page of an experiment that is still a draft: the sentence it came from, what its
 * planner is doing or proposed, and the two things a person does with a proposal, accepting it
 * at a cost they state or asking for a change. A reader of a shared draft is shown neither.
 */
export function DraftView({ draft, readOnly = false }: { draft: Draft; readOnly?: boolean }) {
  const router = useProjectRouter()
  const { mutate } = useSWRConfig()
  const [abandonError, setAbandonError] = useState<string | null>(null)
  if (!draft.status_reason) throw new Error(`draft ${draft.id} came without a status_reason`)
  const state = draft.status_reason.reason
  const reread = () => void mutate(['pe', draft.id])

  async function abandon() {
    setAbandonError(null)
    try {
      await abandonDraft(draft.id)
      reread()
    } catch (e) {
      setAbandonError(said(e))
    }
  }

  return (
    <div>
      <PageHeader
        back={{ label: 'Experiments', onClick: () => router.push('/platform-experiments') }}
        title={draft.name}
        description={<span className="text-xs text-subtle">Draft, {state === 'proposed' || state === 'declined' ? 'waiting for you' : 'being planned'}</span>}
      />
      <Card>
        <CardHeader>What you asked</CardHeader>
        <CardContent className="grid gap-2">
          <p className="text-sm">{draft.goal}</p>
          {(draft.revisions ?? []).map(revision => (
            <p key={revision.at} className="text-sm">
              <span className="text-xs text-muted">{actorLabel(revision.actor)} asked {relTime(revision.at)}: </span>
              {revision.message}
            </p>
          ))}
        </CardContent>
      </Card>
      {(state === 'planning' || state === 'revising') && <Planning draft={draft} revising={state === 'revising'} />}
      {draft.declined_reason && (
        <Card>
          <CardHeader>The planner proposed no experiment</CardHeader>
          <CardContent className="grid gap-2">
            <p className="text-sm">{draft.declined_reason}</p>
            {state === 'declined' && (
              <p className="text-xs text-muted">
                Nothing runs and nothing is spent. Answer below to have it try again, or abandon this draft; it is closed on {formatDate(draft.proposal_expires_at)} otherwise.
              </p>
            )}
          </CardContent>
        </Card>
      )}
      {draft.proposal && (
        <ProposalForm key={draft.proposed_at} draft={draft} proposal={draft.proposal} editable={!readOnly && state === 'proposed'} onDone={reread} />
      )}
      {!readOnly && (state === 'proposed' || state === 'declined') && <Revise draft={draft} declined={state === 'declined'} onDone={reread} />}
      {!readOnly && (
        <div className="mt-2 flex items-center gap-3">
          <Button variant="ghost" onClick={abandon}>Abandon this draft</Button>
          {abandonError && <InlineError>{abandonError}</InlineError>}
        </div>
      )}
    </div>
  )
}

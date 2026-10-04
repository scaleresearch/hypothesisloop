'use client'

import { useRef, useState } from 'react'
import { useSWRConfig } from 'swr'
import { formatDate, relTime } from '@/lib/format'
import { abandonDraft, reviseDraft } from '@/lib/planning'
import type { Draft } from '@/lib/planning'
import { reasonOf } from '@/lib/phase'
import { useProjectRouter } from '@/lib/query-state'
import { actorLabel } from '@/components/actor'
import { PhaseStrip } from '@/components/phase-strip'
import { ProposalForm } from '@/components/proposal-form'
import { Button } from '@/components/ui/button'
import { FormField } from '@/components/ui/form-field'
import { Textarea } from '@/components/ui/input'
import { Page } from '@/components/ui/page'
import { Section } from '@/components/ui/section'
import { Inline, Stack } from '@/components/ui/stack'
import { InlineError } from '@/components/ui/status-message'
import { Text } from '@/components/ui/text'

const said = (error: unknown) => (error instanceof Error ? error.message : String(error))

function Revise({ draft, declined, onDone }: { draft: Draft; declined: boolean; onDone: () => void }) {
  const [message, setMessage] = useState('')
  const [sending, setSending] = useState(false)
  const [error, setError] = useState<string | null>(null)
  // The planner's machine is kept for a while after it answers, read anew at each read of the draft.
  const waiting = !!draft.planner_ends_at && Date.now() < new Date(draft.planner_ends_at).getTime()

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
    <Section title={declined ? 'Answer the planner' : 'Ask for a change'}>
      <Stack gap="sm">
        <FormField
          label={declined ? 'What was missing?' : 'What should be different?'}
          hint={`${waiting
            ? `The planner is still running until ${formatDate(draft.planner_ends_at)} and starts on this at once.`
            : 'The planner starts again first, usually about a minute.'}${declined ? '' : ' The current plan stays until the new one is ready.'}`}
        >
          <Textarea
            rows={2}
            value={message}
            onChange={e => setMessage(e.target.value)}
            placeholder={declined ? 'e.g. The small transformer in train.py, judged by validation loss.' : 'e.g. It must fit a single RTX 4090. Measure latency too.'}
          />
        </FormField>
        <Inline gap="md">
          <Button loading={sending} disabled={message.trim() === ''} onClick={revise}>Revise</Button>
          {error && <InlineError>{error}</InlineError>}
        </Inline>
      </Stack>
    </Section>
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
  const state = reasonOf(draft).reason
  // Whether this page, since it loaded, saw the planner at work: a proposal that follows arrived
  // in front of the person, and one already there on load did not.
  const watchedPlanning = useRef(false)
  if (state === 'planning' || state === 'revising') watchedPlanning.current = true
  const reread = () => void mutate(['pe', draft.id])
  const revise = !readOnly && (state === 'proposed' || state === 'declined') && <Revise draft={draft} declined={state === 'declined'} onDone={reread} />

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
    <Page
      back={{ label: 'Experiments', onClick: () => router.push('/platform-experiments') }}
      title={draft.name}
      description={<Text as="span" variant="hint">Draft, {state === 'proposed' || state === 'declined' ? 'waiting for you' : 'being planned'}</Text>}
    >
      <PhaseStrip pe={draft} readOnly={readOnly} />
      <Section title="What you asked">
        <Stack gap="sm">
          <Text>{draft.goal}</Text>
          {(draft.revisions ?? []).map(revision => (
            <Text key={revision.at}>
              <Text as="span" variant="note">{actorLabel(revision.actor)} asked {relTime(revision.at)}: </Text>
              {revision.message}
            </Text>
          ))}
        </Stack>
      </Section>
      {draft.declined_reason && (
        <Section title="What the planner needs">
          <Stack gap="sm">
            <Text>{draft.declined_reason}</Text>
            {state === 'declined' && (
              <Text variant="note">
                Answer below and it tries again. Otherwise the draft closes on {formatDate(draft.proposal_expires_at)}.
              </Text>
            )}
          </Stack>
        </Section>
      )}
      {draft.proposal ? (
        <ProposalForm key={draft.proposed_at} draft={draft} proposal={draft.proposal} editable={!readOnly && state === 'proposed'} arrived={watchedPlanning.current} onDone={reread}>
          {revise}
        </ProposalForm>
      ) : revise}
      {!readOnly && (
        <Inline gap="md">
          <Button variant="ghost" onClick={abandon}>Abandon this draft</Button>
          {abandonError && <InlineError>{abandonError}</InlineError>}
        </Inline>
      )}
    </Page>
  )
}

'use client'

import { useState } from 'react'
import { useSWRConfig } from 'swr'
import { formatDate, relTime } from '@/lib/format'
import { PLANNING_MOVES, abandonDraft, reviseDraft } from '@/lib/planning'
import type { Draft } from '@/lib/planning'
import { reasonOf } from '@/lib/phase'
import { useRouter } from 'next/navigation'
import { projectPath } from '@/lib/routes'
import { useTenant } from '@/lib/tenant'
import { actorLabel } from '@/components/actor'
import { PhaseStrip } from '@/components/phase-strip'
import { ProposalForm } from '@/components/proposal-form'
import { Button } from '@/components/ui/button'
import { ConfirmDialog } from '@/components/ui/dialog'
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
 * planner is doing or proposed, and what a person does with a proposal: let the platform accept
 * it as proposed, accept it now, edit it and accept it at a cost they state, ask for a change, or
 * reject it. A reader of a shared draft is shown none of these.
 */
export function DraftView({ draft, readOnly = false }: { draft: Draft; readOnly?: boolean }) {
  const router = useRouter()
  const { project } = useTenant()
  const { mutate } = useSWRConfig()
  const [abandonError, setAbandonError] = useState<string | null>(null)
  const [confirming, setConfirming] = useState(false)
  const state = reasonOf(draft).reason
  const reread = () => void mutate(['pe', draft.id])
  const revise = !readOnly && (state === 'proposed' || state === 'accepting' || state === 'declined') && <Revise draft={draft} declined={state === 'declined'} onDone={reread} />

  async function abandon() {
    setConfirming(false)
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
      back={{ label: 'Experiments', onClick: () => router.push(projectPath(project.id, 'experiments')) }}
      title={draft.name}
      description={<Text as="span" variant="hint">Draft, {state === 'proposed' || state === 'declined' ? 'waiting for you' : state === 'accepting' ? 'starting in a moment' : PLANNING_MOVES.has(state) ? 'being planned' : 'planning on hold'}</Text>}
    >
      <PhaseStrip pe={draft} readOnly={readOnly} />
      <Section title="What you asked">
        <Stack gap="sm">
          <Text className="whitespace-pre-line">{draft.goal}</Text>
          {(draft.revisions ?? []).map(revision => (
            <Text key={revision.at} className="whitespace-pre-line">
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
        <ProposalForm key={draft.proposed_at} draft={draft} proposal={draft.proposal} editable={!readOnly && state === 'proposed'} accepting={!readOnly && state === 'accepting'} onDone={reread}>
          {revise}
        </ProposalForm>
      ) : revise}
      {!readOnly && (
        <Inline gap="md">
          <Button variant="ghost" onClick={() => setConfirming(true)}>Abandon this draft</Button>
          {abandonError && <InlineError>{abandonError}</InlineError>}
          <ConfirmDialog open={confirming} title="Abandon this draft?" confirmLabel="Abandon draft" onConfirm={() => void abandon()} onCancel={() => setConfirming(false)}>
            Planning stops and the draft closes. Nothing ran, so nothing is charged, and what you asked stays readable here.
          </ConfirmDialog>
        </Inline>
      )}
    </Page>
  )
}

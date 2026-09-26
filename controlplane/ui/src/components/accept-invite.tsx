'use client'

import { useEffect, useState } from 'react'
import type { ReactNode } from 'react'
import { useRouter } from 'next/navigation'
import { ApiError, acceptOrgInvite } from '@/lib/api'
import { HOME_PATH, LOGIN_PATH, accessToken } from '@/lib/auth'
import { INVITE_PATH, dropInvite, heldInvite } from '@/lib/invite'
import { useRefreshMe } from '@/lib/tenant'
import type { OrgMember } from '@/types'
import { Button } from '@/components/ui/button'
import { Logo } from '@/components/ui/logo'
import { ErrorMessage, Loading } from '@/components/ui/status-message'

const NEW_INVITE = 'Ask an owner of the organization for a new one.'

// Why an invite could not be accepted, by the API's reason. Anything else is shown in the API's words.
const REFUSALS: Record<string, string> = {
  invite_used: `This invite was already accepted, and an invite works once. ${NEW_INVITE}`,
  invite_revoked: `This invite was revoked. ${NEW_INVITE}`,
  invite_expired: `This invite has expired. ${NEW_INVITE}`,
  already_member: 'You are already a member of this organization. The invite was not used and still works for someone else.',
  not_found: 'This invite link is not valid. Check that the whole link was copied, or ask an owner of the organization for a new one.',
}

type Step =
  | { at: 'reading' }
  | { at: 'no-invite' }
  | { at: 'signed-out' }
  | { at: 'offered'; token: string; failure?: string }
  | { at: 'accepting' }
  | { at: 'refused'; message: string }
  | { at: 'joined'; member: OrgMember; stranded?: string }

function Frame({ children }: { children: ReactNode }) {
  return (
    <main className="mx-auto mt-[18vh] grid w-full max-w-sm content-start gap-4 px-5">
      <Logo />
      <h1 className="text-2xl font-bold tracking-tight">Join an organization</h1>
      {children}
    </main>
  )
}

/** The page an invite link opens: signs the person in if need be, then accepts the invite once. */
export function AcceptInvite() {
  const router = useRouter()
  const refreshMe = useRefreshMe()
  const [step, setStep] = useState<Step>({ at: 'reading' })

  useEffect(() => {
    let current = true
    const token = heldInvite()
    if (!token) {
      setStep({ at: 'no-invite' })
      return
    }
    accessToken().then(
      session => { if (current) setStep(session ? { at: 'offered', token } : { at: 'signed-out' }) },
      (e: unknown) => {
        if (current) setStep({ at: 'offered', token, failure: `Could not check your session: ${e instanceof Error ? e.message : String(e)}` })
      },
    )
    return () => { current = false }
  }, [])

  async function accept(token: string) {
    setStep({ at: 'accepting' })
    let member: OrgMember
    try {
      member = await acceptOrgInvite(token)
    } catch (e) {
      if (e instanceof ApiError) {
        dropInvite()
        setStep({ at: 'refused', message: REFUSALS[e.reason ?? (e.status === 404 ? 'not_found' : '')] ?? e.message })
      } else {
        // The API never answered, so the invite is unused and the same token can be sent again.
        setStep({ at: 'offered', token, failure: `The invite could not be sent: ${e instanceof Error ? e.message : String(e)}` })
      }
      return
    }
    dropInvite()
    setStep({ at: 'joined', member })
    try {
      const project = (await refreshMe()).projects.find(p => p.org_id === member.org_id)
      if (project) router.replace(`${HOME_PATH}?project=${project.id}`)
      else setStep({ at: 'joined', member, stranded: 'The organization has no project yet.' })
    } catch (e) {
      setStep({ at: 'joined', member, stranded: `Your account could not be read again: ${e instanceof Error ? e.message : String(e)}` })
    }
  }

  const home = <Button onClick={() => router.replace(HOME_PATH)}>Go to experiments</Button>

  switch (step.at) {
    case 'reading':
      return <Frame><Loading text="Reading the invite…" /></Frame>
    case 'no-invite':
      return (
        <Frame>
          <ErrorMessage>This page holds no invite. Open the whole invite link you were sent, in this tab.</ErrorMessage>
          {home}
        </Frame>
      )
    case 'signed-out':
      return (
        <Frame>
          <p className="text-sm text-muted">You were invited to an organization on HypothesisLoop. Sign in to accept: you come back here afterwards.</p>
          <Button variant="primary" onClick={() => router.push(`${LOGIN_PATH}?${new URLSearchParams({ next: INVITE_PATH })}`)}>
            Sign in to accept
          </Button>
        </Frame>
      )
    case 'offered':
      return (
        <Frame>
          <p className="text-sm text-muted">
            You were invited to an organization on HypothesisLoop. Accepting adds you to it with the role its owner chose
            for this invite, which you see once you have joined.
          </p>
          {step.failure && <ErrorMessage>{step.failure}</ErrorMessage>}
          <Button variant="primary" onClick={() => accept(step.token)}>{step.failure ? 'Try again' : 'Accept invite'}</Button>
        </Frame>
      )
    case 'accepting':
      return <Frame><Loading text="Accepting the invite…" /></Frame>
    case 'refused':
      return (
        <Frame>
          <ErrorMessage>{step.message}</ErrorMessage>
          {home}
        </Frame>
      )
    case 'joined':
      return (
        <Frame>
          <p className="text-sm text-muted" role="status">
            You joined as {step.member.role === 'owner' ? 'an owner' : 'a member'}.{!step.stranded && ' Opening the organization…'}
          </p>
          {step.stranded && (
            <>
              <ErrorMessage>{step.stranded}</ErrorMessage>
              {home}
            </>
          )}
        </Frame>
      )
  }
}

'use client'

import { Suspense, useState } from 'react'
import NextLink from 'next/link'
import { useSearchParams } from 'next/navigation'
import { LANDING_PATH, LOGIN_PATH, PROVIDER_NAMES, ProviderRefused, safeNext, signIn, signInProviders } from '@/lib/auth'
import type { SignInProvider } from '@/lib/auth'
import { BrandMark } from '@/components/ui/brand-mark'
import { Button } from '@/components/ui/button'
import { Centered } from '@/components/ui/centered'
import { inlineLinkClass } from '@/components/ui/link'
import { Logo } from '@/components/ui/logo'
import { Stack } from '@/components/ui/stack'
import { ErrorMessage } from '@/components/ui/status-message'
import { Text } from '@/components/ui/text'
import { errorMessage } from '@/lib/errors'

// Why the visitor is here again, as the redirect that brought them says it.
const NOTICES = new Map([
  ['ended', 'Your session ended. Sign in again to continue where you were.'],
  ['refused', 'The API did not accept your session. Sign in again.'],
  ['failed', 'Sign-in did not complete. Try again.'],
])

const WORDING = {
  'sign-in': {
    title: 'Sign in',
    lead: 'Run experiments, watch the agents work, and manage your organization\'s projects, keys and clusters.',
    other: { question: 'New to HypothesisLoop?', label: 'Sign up', mode: 'sign-up' },
  },
  'sign-up': {
    title: 'Create your account',
    lead: 'Sign up with an account you already have. There is no password to choose, and the same button signs you in next time.',
    other: { question: 'Already have an account?', label: 'Sign in', mode: 'sign-in' },
  },
} as const

function configured(): { providers: SignInProvider[]; problem?: string } {
  try {
    return { providers: signInProviders() }
  } catch (e) {
    return { providers: [], problem: errorMessage(e) }
  }
}

function SignIn() {
  const params = useSearchParams()
  const next = safeNext(params.get('next'))
  const notice = NOTICES.get(params.get('notice') ?? '')
  const wording = WORDING[params.get('mode') === 'sign-up' ? 'sign-up' : 'sign-in']
  const { providers, problem } = configured()
  const [pending, setPending] = useState('')
  const [error, setError] = useState('')
  // Providers the identity provider turned down in this visit, so the same dead button is not offered twice.
  const [refused, setRefused] = useState<SignInProvider[]>([])

  async function start(provider: SignInProvider) {
    setPending(provider)
    setError('')
    try {
      await signIn(provider, next)
    } catch (e) {
      if (e instanceof ProviderRefused) {
        setRefused(list => [...list, provider])
        setError(e.message)
      } else {
        setError(`Sign-in could not start: ${errorMessage(e)}`)
      }
      setPending('')
    }
  }

  // The way back survives a switch between the two wordings.
  const other = new URLSearchParams(params.get('next') ? { next } : {})
  if (wording.other.mode === 'sign-up') other.set('mode', 'sign-up')
  const otherHref = other.toString() ? `${LOGIN_PATH}?${other}` : LOGIN_PATH

  return (
    <Centered head={<NextLink href={LANDING_PATH} className="justify-self-start rounded"><Logo /></NextLink>} title={wording.title}>
      <Text tone="muted">{wording.lead}</Text>
      {notice && <ErrorMessage>{notice}</ErrorMessage>}
      {problem && <ErrorMessage>Sign-in is not configured. {problem}</ErrorMessage>}
      {error && <ErrorMessage>{error}</ErrorMessage>}
      <Stack gap="sm">
        {providers.map(provider => (
          <Button
            key={provider}
            disabled={pending !== '' || refused.includes(provider)}
            loading={pending === provider}
            onClick={() => start(provider)}
          >
            {pending !== provider && <BrandMark provider={provider} />}
            Continue with {PROVIDER_NAMES[provider]}
          </Button>
        ))}
      </Stack>
      <Text variant="hint">
        {wording.other.question}{' '}
        <NextLink href={otherHref} className={inlineLinkClass}>{wording.other.label}</NextLink>
      </Text>
    </Centered>
  )
}

export default function LoginPage() {
  return (
    <Suspense>
      <SignIn />
    </Suspense>
  )
}

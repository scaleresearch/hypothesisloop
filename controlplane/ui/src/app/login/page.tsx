'use client'

import { Suspense, useState } from 'react'
import { useSearchParams } from 'next/navigation'
import { SIGN_IN_METHODS, safeNext, signIn } from '@/lib/auth'
import { Button } from '@/components/ui/button'
import { ErrorMessage } from '@/components/ui/status-message'

// Why the visitor is here again, as the redirect that brought them says it.
const NOTICES = new Map([
  ['ended', 'Your session ended. Sign in again to continue where you were.'],
  ['refused', 'The API did not accept your session. Sign in again.'],
  ['failed', 'Sign-in did not complete. Try again.'],
])

function SignIn() {
  const params = useSearchParams()
  const next = safeNext(params.get('next'))
  const notice = NOTICES.get(params.get('notice') ?? '')
  const [pending, setPending] = useState('')
  const [error, setError] = useState('')

  async function start(provider: (typeof SIGN_IN_METHODS)[number]['provider']) {
    setPending(provider)
    setError('')
    try {
      await signIn(provider, next)
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
      setPending('')
    }
  }

  return (
    <main className="login">
      <div className="wa-nav-logo" style={{ padding: 0 }}>
        <span className="wa-nav-mark">O</span>
        <span>HypothesisLoop</span>
      </div>
      <h1>Sign in</h1>
      <p className="text-muted">Run platform experiments, watch the agents work, and manage your organization&apos;s projects, keys and clusters.</p>
      {notice && <ErrorMessage>{notice}</ErrorMessage>}
      {error && <ErrorMessage>Sign-in could not start: {error}</ErrorMessage>}
      {SIGN_IN_METHODS.map(({ provider, label }) => (
        <Button key={provider} variant="primary" disabled={pending !== ''} onClick={() => start(provider)}>
          {pending === provider ? 'Redirecting…' : label}
        </Button>
      ))}
    </main>
  )
}

export default function LoginPage() {
  return (
    <Suspense>
      <SignIn />
    </Suspense>
  )
}

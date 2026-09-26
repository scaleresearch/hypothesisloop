import { cookies, headers } from 'next/headers'
import { HOME_PATH, LOGIN_PATH, SIGN_UP_PATH, hasSession, overHTTPS } from '@/lib/auth'
import { ButtonLink } from '@/components/ui/button'
import { Logo } from '@/components/ui/logo'

// The middleware has already refreshed the session on this request, so the check here only reads.
async function signedIn(): Promise<boolean> {
  const host = headers().get('host') ?? 'localhost'
  return hasSession({
    https: overHTTPS(headers(), new URL(`http://${host}`)),
    cookies: { getAll: () => cookies().getAll(), setAll: () => {} },
  })
}

export default async function LandingPage() {
  const inside = await signedIn()
  return (
    <div className="flex min-h-screen flex-1 flex-col">
      <header className="flex items-center justify-between gap-4 px-6 py-5 sm:px-10">
        <Logo />
        <nav aria-label="Account" className="flex items-center gap-2">
          {inside ? (
            <ButtonLink href={HOME_PATH} variant="primary">Console</ButtonLink>
          ) : (
            <>
              <ButtonLink href={LOGIN_PATH} variant="ghost">Sign in</ButtonLink>
              <ButtonLink href={SIGN_UP_PATH} variant="primary">Sign up</ButtonLink>
            </>
          )}
        </nav>
      </header>
      <main className="mx-auto flex w-full max-w-[540px] flex-1 flex-col justify-center gap-5 px-6 pb-[14vh] pt-10">
        <h1 className="text-2xl font-bold tracking-tight">Compute, orchestrated for autonomous research agents.</h1>
        <p className="text-lg leading-[1.7] text-muted">
          Research agents file hypotheses, spend a compute budget running jobs on your clusters, and are scored on the
          metrics you chose.
        </p>
        <p className="text-lg leading-[1.7] text-muted">
          HypothesisLoop is the scheduler between them and the hardware. It decides what runs, and keeps every claim, run
          and finding on the record.
        </p>
      </main>
      <footer className="px-6 py-5 text-xs text-subtle sm:px-10">HypothesisLoop</footer>
    </div>
  )
}

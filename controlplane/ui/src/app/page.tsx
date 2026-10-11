import { cookies, headers } from 'next/headers'
import { HOME_PATH, LOGIN_PATH, SIGN_UP_PATH, hasSession, overHTTPS } from '@/lib/auth'
import { ButtonLink } from '@/components/ui/button'
import { Logo } from '@/components/ui/logo'
import { Heading } from '@/components/ui/text'

// The middleware has already refreshed the session on this request, so the check here only reads.
async function signedIn(): Promise<boolean> {
  const host = headers().get('host') ?? 'localhost'
  return hasSession({
    https: overHTTPS(headers(), new URL(`http://${host}`)),
    cookies: { getAll: () => cookies().getAll(), setAll: () => { } },
  })
}

export default async function LandingPage() {
  const inside = await signedIn()
  return (
    <div className="flex min-h-screen flex-1 flex-col">
      <header className="flex items-center justify-end gap-4 px-6 py-5 sm:px-10">
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
      <main className="mx-auto flex w-full max-w-xl flex-1 flex-col items-center justify-center gap-5 px-6 text-center pb-drop pt-10">
        <Heading level={1}>Autonomous research</Heading>
      </main>
    </div>
  )
}

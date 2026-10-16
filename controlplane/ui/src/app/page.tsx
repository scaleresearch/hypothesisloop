import { headers } from 'next/headers'
import { HOME_PATH, LOGIN_PATH, SIGNED_IN_HEADER, SIGN_UP_PATH } from '@/lib/auth'
import { ButtonLink } from '@/components/ui/button'
import { Heading } from '@/components/ui/text'

export default function LandingPage() {
  const inside = headers().get(SIGNED_IN_HEADER) === 'yes'
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

import { NextResponse, type NextRequest } from 'next/server'
import { LANDING_PATH, LOGIN_PATH, SIGNED_IN_HEADER, hasSession, overHTTPS, safeNext } from '@/lib/auth'
import type { ServerSession } from '@/lib/auth'
import { INVITE_PATH } from '@/lib/invite'
import { NONCE_HEADER, contentSecurityPolicy, newNonce } from '@/lib/content-security'

type WrittenCookie = Parameters<NextResponse['cookies']['set']>[2] & { name: string; value: string }

// An invite link carries its token in the fragment, which a redirect through sign-in would lose,
// so its page is served to anyone and sends a visitor without a session to sign in itself.
const PUBLIC_PATHS = new Set([LANDING_PATH, LOGIN_PATH, INVITE_PATH])

// Runs before every page: refreshes the session's cookies, sends a visitor without a session to
// the sign-in page unless the page is public, and a signed-in one away from sign-in.
export async function middleware(request: NextRequest) {
  const cookies: WrittenCookie[] = []
  const headers: Record<string, string> = {}
  const session: ServerSession = {
    https: overHTTPS(request.headers, request.nextUrl),
    cookies: {
      getAll: () => request.cookies.getAll(),
      setAll: (written, cacheHeaders) => {
        for (const { name, value, options } of written) {
          request.cookies.set(name, value)
          cookies.push({ ...options, name, value })
        }
        Object.assign(headers, cacheHeaders)
      },
    },
  }
  const signedIn = await hasSession(session)
  // The pages behind this read the answer here rather than asking the identity provider again.
  // Set on every request, so no visitor can send it.
  request.headers.set(SIGNED_IN_HEADER, signedIn ? 'yes' : 'no')
  const nonce = newNonce()
  const policy = contentSecurityPolicy(nonce, process.env.NODE_ENV === 'development')
  request.headers.set(NONCE_HEADER, nonce)
  request.headers.set('Content-Security-Policy', policy)

  const { pathname, search, searchParams } = request.nextUrl
  let response: NextResponse
  if (!signedIn && !PUBLIC_PATHS.has(pathname)) {
    const login = request.nextUrl.clone()
    login.pathname = LOGIN_PATH
    login.search = new URLSearchParams({ next: pathname + search }).toString()
    response = NextResponse.redirect(login)
  } else if (signedIn && pathname === LOGIN_PATH) {
    response = NextResponse.redirect(new URL(safeNext(searchParams.get('next')), request.nextUrl))
  } else {
    response = NextResponse.next({ request })
  }

  // Whatever the session check wrote, a refreshed token or a cleared one, leaves with the answer.
  for (const cookie of cookies) response.cookies.set(cookie)
  for (const [name, value] of Object.entries(headers)) response.headers.set(name, value)
  response.headers.set('Content-Security-Policy', policy)
  if (session.https) response.headers.set('Strict-Transport-Security', 'max-age=63072000; includeSubDomains')
  return response
}

export const config = {
  // The callback route signs the visitor in, so it is reached without a session.
  matcher: ['/((?!_next/static|_next/image|favicon.ico|auth/callback).*)'],
}

import { NextResponse, type NextRequest } from 'next/server'
import { LANDING_PATH, LOGIN_PATH, hasSession, overHTTPS, safeNext, sessionToken } from '@/lib/auth'
import type { ServerSession } from '@/lib/auth'
import { movedTo, recordPath } from '@/lib/legacy-routes'
import { INVITE_PATH } from '@/lib/invite'

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

  const { pathname, search, searchParams } = request.nextUrl
  let response: NextResponse
  let moved: ReturnType<typeof movedTo>
  if (!signedIn && !PUBLIC_PATHS.has(pathname)) {
    const login = request.nextUrl.clone()
    login.pathname = LOGIN_PATH
    login.search = new URLSearchParams({ next: pathname + search }).toString()
    response = NextResponse.redirect(login)
  } else if (signedIn && pathname === LOGIN_PATH) {
    response = NextResponse.redirect(new URL(safeNext(searchParams.get('next')), request.nextUrl))
  } else if (signedIn && (moved = movedTo(pathname, searchParams))) {
    const path = 'path' in moved ? moved.path : await recordMovedTo(moved.record, moved.id, session)
    response = path ? NextResponse.redirect(new URL(path, request.nextUrl), 308) : NextResponse.next({ request })
  } else {
    response = NextResponse.next({ request })
  }

  // Whatever the session check wrote, a refreshed token or a cleared one, leaves with the answer.
  for (const cookie of cookies) response.cookies.set(cookie)
  for (const [name, value] of Object.entries(headers)) response.headers.set(name, value)
  return response
}

// A job or hypothesis keeps its experiment for good, so its old link moves as permanently as the
// rest. One the caller cannot read is left to answer not found.
async function recordMovedTo(record: 'job' | 'hypothesis', id: string, session: ServerSession): Promise<string | null> {
  const token = await sessionToken(session)
  if (!token) return null
  const api = process.env.NEXT_PUBLIC_API_URL
  if (!api) throw new Error('NEXT_PUBLIC_API_URL is not set: copy .env.example to .env.local and fill it in')
  const res = await fetch(`${api}/${record === 'job' ? 'experiments' : 'hypotheses'}/${encodeURIComponent(id)}`, {
    headers: { Authorization: `Bearer ${token}` },
    cache: 'no-store',
  })
  if (!res.ok) return null
  const { platform_experiment_id: experimentID } = await res.json() as { platform_experiment_id: string }
  return recordPath(record, id, experimentID)
}

export const config = {
  // The callback route signs the visitor in, so it is reached without a session.
  matcher: ['/((?!_next/static|_next/image|favicon.ico|auth/callback).*)'],
}

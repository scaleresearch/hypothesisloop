import { NextResponse, type NextRequest } from 'next/server'
import { LOGIN_PATH, hasSession, safeNext } from '@/lib/auth'

type WrittenCookie = Parameters<NextResponse['cookies']['set']>[2] & { name: string; value: string }

// Runs before every page: refreshes the session's cookies, sends a visitor without a session to
// the sign-in page, and a signed-in one away from it.
export async function middleware(request: NextRequest) {
  const cookies: WrittenCookie[] = []
  const headers: Record<string, string> = {}
  const signedIn = await hasSession({
    getAll: () => request.cookies.getAll(),
    setAll: (written, cacheHeaders) => {
      for (const { name, value, options } of written) {
        request.cookies.set(name, value)
        cookies.push({ ...options, name, value })
      }
      Object.assign(headers, cacheHeaders)
    },
  })

  const { pathname, search, searchParams } = request.nextUrl
  let response: NextResponse
  if (!signedIn && pathname !== LOGIN_PATH) {
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
  return response
}

export const config = {
  // The callback route signs the visitor in, so it is reached without a session.
  matcher: ['/((?!_next/static|_next/image|favicon.ico|auth/callback).*)'],
}

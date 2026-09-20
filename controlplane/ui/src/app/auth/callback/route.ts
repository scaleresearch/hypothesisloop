import { NextResponse, type NextRequest } from 'next/server'
import { LOGIN_PATH, completeSignIn, safeNext } from '@/lib/auth'

// Where the identity provider sends the browser back to after sign-in. The code it carries is
// traded for a session, whose cookies leave with the redirect into the app. The redirect is
// relative, so it holds behind a proxy that hides the public host from this server.
export async function GET(request: NextRequest) {
  const params = request.nextUrl.searchParams
  const code = params.get('code')
  const response = new NextResponse(null, { status: 303 })
  const signedIn = code != null && await completeSignIn({
    getAll: () => request.cookies.getAll(),
    setAll: (written, cacheHeaders) => {
      for (const { name, value, options } of written) response.cookies.set(name, value, options)
      for (const [name, value] of Object.entries(cacheHeaders)) response.headers.set(name, value)
    },
  }, code)
  response.headers.set('Location', signedIn ? safeNext(params.get('next')) : `${LOGIN_PATH}?notice=failed`)
  return response
}

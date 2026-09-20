import { createHash, createPrivateKey, createPublicKey, randomUUID, sign } from 'node:crypto'
import { readFileSync } from 'node:fs'
import { createServer } from 'node:http'

const KID = 'local-dev'
const ISSUER = 'http://localhost/hypothesisloop-dev'

const base64url = value => Buffer.from(value).toString('base64url')

export function startIdentityProvider({ port, keyPath, ui }) {
  const privateKey = createPrivateKey(readFileSync(keyPath))
  const jwk = { ...createPublicKey(privateKey).export({ format: 'jwk' }), kid: KID, alg: 'ES256', use: 'sig' }

  const state = {
    nextUser: null,
    tokenLifetimeSeconds: 3600,
    audience: 'hypothesisloop-dev',
    codes: new Map(),
    refreshTokens: new Map(),
    refreshes: 0,
    authorizeRequests: [],
    githubCodes: [],
    githubDeclines: false,
  }

  function accessToken(user) {
    const now = Math.floor(Date.now() / 1000)
    const header = base64url(JSON.stringify({ alg: 'ES256', typ: 'JWT', kid: KID }))
    const claims = base64url(JSON.stringify({
      iss: ISSUER, aud: state.audience, sub: user.id, iat: now, exp: now + state.tokenLifetimeSeconds,
      role: 'authenticated', is_anonymous: false, session_id: randomUUID(),
      user_metadata: { name: user.name },
    }))
    const signature = sign('sha256', Buffer.from(`${header}.${claims}`), { key: privateKey, dsaEncoding: 'ieee-p1363' })
    return `${header}.${claims}.${base64url(signature)}`
  }

  function session(user) {
    const refreshToken = randomUUID()
    state.refreshTokens.set(refreshToken, user)
    return {
      access_token: accessToken(user),
      token_type: 'bearer',
      expires_in: state.tokenLifetimeSeconds,
      expires_at: Math.floor(Date.now() / 1000) + state.tokenLifetimeSeconds,
      refresh_token: refreshToken,
      user: { id: user.id, aud: 'authenticated', role: 'authenticated', user_metadata: { name: user.name }, app_metadata: {} },
    }
  }

  const server = createServer(async (req, res) => {
    const url = new URL(req.url, `http://127.0.0.1:${port}`)
    const chunks = []
    for await (const chunk of req) chunks.push(chunk)
    const body = chunks.length ? JSON.parse(Buffer.concat(chunks).toString()) : {}

    res.setHeader('Access-Control-Allow-Origin', req.headers.origin ?? '*')
    res.setHeader('Access-Control-Allow-Headers', req.headers['access-control-request-headers'] ?? '*')
    res.setHeader('Access-Control-Allow-Methods', 'GET, POST, OPTIONS')
    const json = (status, value) => {
      res.writeHead(status, { 'Content-Type': 'application/json' })
      res.end(JSON.stringify(value))
    }
    if (req.method === 'OPTIONS') return res.writeHead(204).end()

    const route = `${req.method} ${url.pathname}`
    if (route === 'GET /auth/v1/.well-known/jwks.json') return json(200, { keys: [jwk] })

    if (route === 'GET /auth/v1/authorize') {
      state.authorizeRequests.push(url.toString())
      const code = randomUUID()
      state.codes.set(code, { user: state.nextUser, challenge: url.searchParams.get('code_challenge') })
      const back = new URL(url.searchParams.get('redirect_to'))
      back.searchParams.set('code', code)
      return res.writeHead(302, { Location: back.toString() }).end()
    }

    if (route === 'POST /auth/v1/token' && url.searchParams.get('grant_type') === 'pkce') {
      const pending = state.codes.get(body.auth_code)
      state.codes.delete(body.auth_code)
      const challenge = createHash('sha256').update(body.code_verifier ?? '').digest('base64url')
      if (!pending || pending.challenge !== challenge) return json(400, { code: 'invalid_grant', error_code: 'bad_code_verifier', msg: 'code or verifier does not match' })
      return json(200, session(pending.user))
    }

    if (route === 'POST /auth/v1/token' && url.searchParams.get('grant_type') === 'refresh_token') {
      const user = state.refreshTokens.get(body.refresh_token)
      if (!user) return json(400, { code: 400, error_code: 'refresh_token_not_found', msg: 'Invalid Refresh Token: Refresh Token Not Found' })
      state.refreshTokens.delete(body.refresh_token)
      state.refreshes += 1
      return json(200, session(user))
    }

    if (route === 'POST /auth/v1/logout') return res.writeHead(204).end()

    // GitHub, stood in for. Installing the App returns to the App's setup address, as GitHub
    // does. Authorising returns to redirect_uri with a code, or with the person's refusal.
    if (route === 'GET /github/install') {
      const back = new URL('/settings/repository', ui)
      back.searchParams.set('installation_id', '4242')
      back.searchParams.set('setup_action', 'install')
      return res.writeHead(302, { Location: back.toString() }).end()
    }
    if (route === 'GET /github/authorize') {
      const back = new URL(url.searchParams.get('redirect_uri'))
      back.searchParams.set('state', url.searchParams.get('state'))
      if (state.githubDeclines) {
        back.searchParams.set('error', 'access_denied')
        back.searchParams.set('error_description', 'The user has denied your application access.')
      } else {
        const code = randomUUID()
        state.githubCodes.push(code)
        back.searchParams.set('code', code)
      }
      return res.writeHead(302, { Location: back.toString() }).end()
    }

    // What a test sets before it acts: who signs in next, how long tokens live, what audience
    // they name, and which user's sessions are revoked.
    if (route === 'POST /test/configure') {
      if (body.nextUser) state.nextUser = body.nextUser
      if (body.tokenLifetimeSeconds) state.tokenLifetimeSeconds = body.tokenLifetimeSeconds
      if (body.audience) state.audience = body.audience
      if (body.githubDeclines !== undefined) state.githubDeclines = body.githubDeclines
      if (body.revoke) {
        for (const [token, user] of state.refreshTokens) if (user.id === body.revoke) state.refreshTokens.delete(token)
      }
      return json(200, {})
    }
    if (route === 'GET /test/state') return json(200, { refreshes: state.refreshes, authorizeRequests: state.authorizeRequests, githubCodes: state.githubCodes })
    if (route === 'POST /test/token') return json(200, { access_token: accessToken(body.user) })

    json(404, { msg: `the identity provider stand-in does not answer ${route}` })
  })

  return new Promise(resolve => server.listen(port, '127.0.0.1', () => resolve(server)))
}

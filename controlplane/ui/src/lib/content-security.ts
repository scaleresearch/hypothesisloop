import { apiURL } from '@/lib/api/request'
import { identityServiceURL } from '@/lib/auth'

/** The request header that hands a page the nonce its inline scripts carry. */
export const NONCE_HEADER = 'x-nonce'

// Next.js writes the page's data as inline scripts that differ on every page, so no hash can name
// them: each response gets a fresh nonce, which Next.js reads off the request's policy and puts on
// its scripts. The browser talks to the API and the sign-in service and nothing else.
export function contentSecurityPolicy(nonce: string, development: boolean): string {
  const connect = ["'self'", new URL(apiURL()).origin, new URL(identityServiceURL()).origin]
  // next dev reloads over a websocket and evaluates the modules it sends.
  if (development) connect.push('ws:')
  return [
    "default-src 'self'",
    `script-src 'self' 'nonce-${nonce}'${development ? " 'unsafe-eval'" : ''}`,
    // Charts and measured boxes set style attributes, which no nonce reaches.
    "style-src 'self' 'unsafe-inline'",
    "img-src 'self' data: blob:",
    "font-src 'self'",
    `connect-src ${connect.join(' ')}`,
    "object-src 'none'",
    "base-uri 'self'",
    "form-action 'self'",
    "frame-ancestors 'none'",
  ].join('; ')
}

/** A nonce of 128 random bits. */
export function newNonce(): string {
  return btoa(Array.from(crypto.getRandomValues(new Uint8Array(16)), b => String.fromCharCode(b)).join(''))
}

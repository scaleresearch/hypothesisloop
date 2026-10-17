import { PHASE_DEVELOPMENT_SERVER, PHASE_PRODUCTION_SERVER } from 'next/constants.js'

// What src/lib/config.ts reads from the environment, so a server started without one is refused
// here, naming it. The build needs none of them.
const required = [
  'UI_API_URL',
  'IDENTITY_PROVIDER',
  'IDENTITY_SUPABASE_URL',
  'IDENTITY_SUPABASE_PUBLISHABLE_KEY',
  'IDENTITY_SIGN_IN_PROVIDERS',
]

export default function nextConfig(phase) {
  if (phase === PHASE_PRODUCTION_SERVER || phase === PHASE_DEVELOPMENT_SERVER) {
    const missing = required.filter(name => !process.env[name])
    if (missing.length > 0) {
      throw new Error(`${missing.join(', ')} ${missing.length === 1 ? 'is' : 'are'} required to serve the UI, as controlplane/ui/.env.example shows`)
    }
  }
  return {
    // The browser e2e run serves its own copy of the UI beside a running one, built into its own directory.
    distDir: process.env.NEXT_DIST_DIR ?? '.next',
    poweredByHeader: false,
    // The content security policy and HSTS depend on the request, so the middleware sets them.
    async headers() {
      return [{
        source: '/:path*',
        headers: [
          { key: 'X-Content-Type-Options', value: 'nosniff' },
          { key: 'Referrer-Policy', value: 'strict-origin-when-cross-origin' },
        ],
      }]
    },
  }
}

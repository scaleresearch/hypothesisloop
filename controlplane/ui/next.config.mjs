import { PHASE_DEVELOPMENT_SERVER, PHASE_PRODUCTION_BUILD } from 'next/constants.js'

// What src/lib/config.ts reads. Next.js compiles each into the bundle, so a UI built or served
// without one is refused here, naming it.
const required = [
  'NEXT_PUBLIC_API_URL',
  'NEXT_PUBLIC_IDENTITY_PROVIDER',
  'NEXT_PUBLIC_IDENTITY_SUPABASE_URL',
  'NEXT_PUBLIC_IDENTITY_SUPABASE_PUBLISHABLE_KEY',
  'NEXT_PUBLIC_SIGN_IN_PROVIDERS',
]

export default function nextConfig(phase) {
  if (phase === PHASE_PRODUCTION_BUILD || phase === PHASE_DEVELOPMENT_SERVER) {
    const missing = required.filter(name => !process.env[name])
    if (missing.length > 0) {
      throw new Error(`${missing.join(', ')} ${missing.length === 1 ? 'is' : 'are'} required to build the UI, as controlplane/ui/.env.example shows`)
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

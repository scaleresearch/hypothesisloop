// The values a deployment runs the UI with, read from the server's environment when it starts
// serving, so one image serves any deployment. The browser gets the same values from the script
// the root layout writes into every page.
const NAMES = ['UI_API_URL', 'IDENTITY_PROVIDER', 'IDENTITY_SUPABASE_URL', 'IDENTITY_SUPABASE_PUBLISHABLE_KEY', 'IDENTITY_SIGN_IN_PROVIDERS'] as const
type Name = typeof NAMES[number]

declare global {
  interface Window {
    __hypothesisloopConfig?: Partial<Record<Name, string>>
  }
}

function read(name: Name): string | undefined {
  if (typeof window === 'undefined') return process.env[name]
  const config = window.__hypothesisloopConfig
  if (!config) throw new Error('the page carries no configuration, which the root layout writes')
  return config[name]
}

export const publicConfig = {
  apiURL: () => read('UI_API_URL'),
  identityProvider: () => read('IDENTITY_PROVIDER'),
  identitySupabaseURL: () => read('IDENTITY_SUPABASE_URL'),
  identitySupabasePublishableKey: () => read('IDENTITY_SUPABASE_PUBLISHABLE_KEY'),
  signInProviders: () => read('IDENTITY_SIGN_IN_PROVIDERS'),
}

/** The inline script that hands the browser the server's values. None is a secret. */
export function configScript(): string {
  const values = Object.fromEntries(NAMES.map(name => [name, process.env[name]]))
  return `window.__hypothesisloopConfig=${JSON.stringify(values).replace(/</g, '\\u003c')}`
}

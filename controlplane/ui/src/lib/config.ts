// What the UI is built with. Next.js inlines a NEXT_PUBLIC_ variable only where it is named in
// full, so each is named here and nowhere else, and next.config.mjs refuses to build without one.
// Each is read when it is asked for.
export const publicConfig = {
  apiURL: () => process.env.NEXT_PUBLIC_API_URL,
  identityProvider: () => process.env.NEXT_PUBLIC_IDENTITY_PROVIDER,
  identitySupabaseURL: () => process.env.NEXT_PUBLIC_IDENTITY_SUPABASE_URL,
  identitySupabasePublishableKey: () => process.env.NEXT_PUBLIC_IDENTITY_SUPABASE_PUBLISHABLE_KEY,
  signInProviders: () => process.env.NEXT_PUBLIC_SIGN_IN_PROVIDERS,
}

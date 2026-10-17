import { fileURLToPath } from 'node:url'
import react from '@vitejs/plugin-react'
import { defineConfig } from 'vitest/config'

export default defineConfig({
  plugins: [react()],
  resolve: { alias: { '@': fileURLToPath(new URL('./src', import.meta.url)) } },
  test: {
    environment: 'jsdom',
    include: ['src/**/*.test.{ts,tsx}'],
    env: {
      UI_API_URL: 'http://api.test',
      IDENTITY_PROVIDER: 'supabase',
      IDENTITY_SUPABASE_URL: 'http://identity.test',
      IDENTITY_SUPABASE_PUBLISHABLE_KEY: 'sb_publishable_test',
      IDENTITY_SIGN_IN_PROVIDERS: 'github,google,apple',
    },
    setupFiles: ['src/test-setup.ts'],
    restoreMocks: true,
    unstubGlobals: true,
  },
})

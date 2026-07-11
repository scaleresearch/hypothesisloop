import type { Config } from 'tailwindcss'

const config: Config = {
  content: [
    './src/app/**/*.{js,ts,jsx,tsx,mdx}',
    './src/components/**/*.{js,ts,jsx,tsx,mdx}',
  ],
  darkMode: 'class',
  theme: {
    extend: {
      colors: {
        background: '#08090c',
        surface: '#0f1116',
        'surface-2': '#14161d',
        'surface-raised': '#191c24',
        foreground: '#f2f3f7',
        border: 'rgba(255, 255, 255, 0.08)',
        'border-dark': 'rgba(255, 255, 255, 0.16)',
        muted: 'rgba(255, 255, 255, 0.045)',
        'muted-fg': '#8d92a6',
        'dim-fg': '#62697e',
        accent: {
          DEFAULT: '#7c6cf0',
          dark: '#5a48d6',
          light: '#a698ff',
        },
        error: '#f2596b',
      },
      fontFamily: {
        sans: ['var(--font-sans)'],
        mono: ['var(--font-mono)'],
      },
      borderRadius: {
        DEFAULT: '12px',
        sm: '8px',
      },
      boxShadow: {
        xs: '0 1px 2px rgba(0, 0, 0, 0.3)',
        sm: '0 1px 2px rgba(0, 0, 0, 0.35), 0 0 0 1px rgba(255, 255, 255, 0.03)',
        md: '0 10px 30px -8px rgba(0, 0, 0, 0.55), 0 2px 8px rgba(0, 0, 0, 0.35)',
        lg: '0 24px 60px -16px rgba(0, 0, 0, 0.65)',
        glow: '0 0 0 1px rgba(124, 108, 240, 0.4), 0 4px 18px rgba(124, 108, 240, 0.35)',
      },
    },
  },
  plugins: [],
}

export default config

import type { Config } from 'tailwindcss'

const token = (name: string) => `rgb(var(--${name}) / <alpha-value>)`

// The whole palette: every colour class resolves to a token in globals.css.
const config: Config = {
  content: [
    './src/app/**/*.{js,ts,jsx,tsx,mdx}',
    './src/components/**/*.{js,ts,jsx,tsx,mdx}',
  ],
  darkMode: 'media',
  theme: {
    colors: {
      transparent: 'transparent',
      current: 'currentColor',
      inherit: 'inherit',
      background: token('background'),
      surface: {
        DEFAULT: token('surface'),
        inset: token('surface-inset'),
        raised: token('surface-raised'),
      },
      border: {
        DEFAULT: token('border'),
        strong: token('border-strong'),
        control: token('border-control'),
      },
      foreground: token('text'),
      muted: token('text-muted'),
      subtle: token('text-subtle'),
      primary: {
        DEFAULT: token('primary'),
        hover: token('primary-hover'),
        foreground: token('primary-foreground'),
      },
      accent: token('accent'),
      success: token('success'),
      warning: token('warning'),
      danger: token('danger'),
      info: token('info'),
      purple: token('purple'),
      pink: token('pink'),
      orange: token('orange'),
      neutral: token('neutral'),
      ring: token('ring'),
      overlay: token('overlay'),
    },
    fontSize: {
      '2xs': ['11px', '1.4'],
      xs: ['12px', '1.5'],
      sm: ['13px', '1.5'],
      base: ['13.5px', '1.55'],
      lg: ['15px', '1.45'],
      xl: ['18px', '1.35'],
      '2xl': ['23px', '1.25'],
      '3xl': ['40px', '1.1'],
    },
    borderRadius: {
      none: '0',
      sm: 'var(--radius-sm)',
      DEFAULT: 'var(--radius)',
      lg: 'var(--radius-lg)',
      full: '9999px',
    },
    boxShadow: {
      none: 'none',
      sm: 'var(--shadow-sm)',
      md: 'var(--shadow-md)',
      lg: 'var(--shadow-lg)',
      glow: 'var(--shadow-glow)',
    },
    extend: {
      fontFamily: {
        sans: ['var(--font-sans)', 'system-ui', 'sans-serif'],
        mono: ['var(--font-mono)', 'ui-monospace', 'monospace'],
      },
      keyframes: {
        rise: {
          from: { opacity: '0', transform: 'translateY(6px)' },
          to: { opacity: '1', transform: 'translateY(0)' },
        },
        shimmer: {
          from: { backgroundPosition: '100% 50%' },
          to: { backgroundPosition: '0% 50%' },
        },
      },
      animation: {
        rise: 'rise 0.28s cubic-bezier(0.22, 1, 0.36, 1) both',
        shimmer: 'shimmer 1.6s ease-in-out infinite',
      },
    },
  },
  plugins: [],
}

export default config

import type { Config } from 'tailwindcss'

const token = (name: string) => `rgb(var(--${name}) / <alpha-value>)`
const state = (name: string, alpha: string) => `rgb(var(--${name}) / var(--alpha-${alpha}))`

// A tone with the fills it is laid on: tint behind its own text (a badge, an error), faint behind
// a note (a callout), edge for the border of either.
const tone = (name: string) => ({
  DEFAULT: token(name),
  tint: state(name, 'tint'),
  faint: state(name, 'faint'),
  edge: state(name, 'edge'),
})

// The whole palette: every colour class resolves to a token in globals.css. There is no opacity
// modifier in the components: a hover, a selection or a tint is one of the named states below.
const config: Config = {
  content: [
    './src/app/**/*.{js,ts,jsx,tsx,mdx}',
    './src/components/**/*.{js,ts,jsx,tsx,mdx}',
  ],
  darkMode: ['selector', '[data-theme="dark"]'],
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
      accent: tone('accent'),
      success: tone('success'),
      warning: tone('warning'),
      danger: tone('danger'),
      info: tone('info'),
      neutral: tone('neutral'),
      chart: Object.fromEntries([1, 2, 3, 4, 5, 6, 7, 8].map(n => [n, token(`chart-${n}`)])),
      ring: token('ring'),
      // The row or button under the pointer.
      hover: state('text', 'hover'),
      // The item a person is on: the current page in the nav, the option the arrow keys reached.
      highlight: state('text', 'highlight'),
      // What a person picked and that stays picked: a row, a toggle, a filter in force.
      selected: state('accent', 'selected'),
      // The empty part of a bar, and a skeleton.
      track: state('text', 'track'),
      // Behind a dialog.
      scrim: state('overlay', 'scrim'),
    },
    opacity: {
      0: '0',
      // A control that cannot be used now.
      disabled: '0.5',
      // A thing set aside: filtered out, or not a choice in the current step.
      dimmed: '0.25',
      100: '1',
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
      },
      animation: {
        rise: 'rise 0.28s cubic-bezier(0.22, 1, 0.36, 1) both',
      },
    },
  },
  plugins: [],
}

export default config

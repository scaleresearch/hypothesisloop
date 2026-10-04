import type { Config } from 'tailwindcss'
import plugin from 'tailwindcss/plugin'
import { fontSize, fontWeight, opacity } from './src/lib/scale'

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

// The named widths of a measure, a panel or a page. min-w takes them too, so a wide table names
// the same step a page does.
const measures = {
  xs: '20rem',
  sm: '24rem',
  md: '28rem',
  lg: '32rem',
  xl: '36rem',
  '2xl': '42rem',
  '3xl': '48rem',
  '4xl': '56rem',
  '5xl': '64rem',
  '6xl': '72rem',
  '7xl': '80rem',
}

// The whole theme. Every colour class resolves to a token in globals.css, and type, weight,
// opacity, radius and shadow are closed scales: a component names a step, never a value.
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
      disabled: String(opacity.disabled),
      dimmed: String(opacity.dimmed),
      100: '1',
    },
    fontSize,
    fontWeight: {
      normal: String(fontWeight.normal),
      medium: String(fontWeight.medium),
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
    fontFamily: {
      sans: ['var(--font-sans)', 'system-ui', 'sans-serif'],
      mono: ['var(--font-mono)', 'ui-monospace', 'monospace'],
    },
    maxWidth: ({ theme }) => ({ ...theme('spacing'), none: 'none', full: '100%', ...measures }),
    // The states Headless UI marks with a data attribute, so a class reads data-open: and so on.
    data: {
      open: 'open',
      focus: 'focus',
      selected: 'selected',
      checked: 'checked',
      disabled: 'disabled',
      invalid: 'invalid',
    },
    extend: {
      spacing: {
        // The vertical padding of a 36px control around a 20px line.
        1.75: '0.4375rem',
        // A mark sized by the text around it.
        em: '1em',
        // How far a lone column (sign-in, the palette) sits below the top of the window.
        drop: '16vh',
      },
      minWidth: {
        ...measures,
        // Headless UI measures a dropdown's field into these.
        listbox: 'var(--button-width)',
        combobox: 'var(--input-width)',
      },
      width: {
        // The side panel of the hypothesis graph, and the idea editor that opens in it.
        aside: '340px',
      },
      minHeight: {
        // The hypothesis workspace fills the window under the page's own padding.
        workspace: 'calc(100vh - 3.5rem)',
        canvas: '420px',
      },
      flex: {
        // Grows from nothing, so a flex column hands it the space left over.
        fill: '1 0 0',
      },
      gridTemplateColumns: {
        'with-aside': 'minmax(0, 1fr) 340px',
        tiles: 'repeat(auto-fit, minmax(11rem, 1fr))',
        'tiles-sm': 'repeat(auto-fit, minmax(7.5rem, 1fr))',
        panels: 'repeat(auto-fill, minmax(30rem, 1fr))',
        // A numbered row of three fields with a control at its end.
        ladder: '1.25rem repeat(3, minmax(0, 1fr)) 1.25rem',
      },
      borderWidth: {
        // The status edge down the side of a graph node.
        3: '3px',
      },
      scale: {
        // A pressed button.
        98: '.98',
      },
      keyframes: {
        rise: {
          from: { opacity: '0', transform: 'translateY(6px)' },
          to: { opacity: '1', transform: 'translateY(0)' },
        },
      },
      animation: {
        rise: 'rise 0.4s cubic-bezier(0.22, 1, 0.36, 1) both',
      },
      transitionDuration: {
        DEFAULT: '200ms',
      },
      transitionTimingFunction: {
        DEFAULT: 'cubic-bezier(0.4, 0, 0.2, 1)',
      },
    },
  },
  plugins: [
    plugin(({ addVariant }) => {
      // A control that can be used now. Written as not-disabled so it also holds for a link,
      // which is never :enabled.
      addVariant('usable', '&:not(:disabled)')
      // Disabled while waiting for input, not busy with its own action (aria-busy).
      addVariant('waiting', '&:disabled:not([aria-busy])')
      // A label whose checkbox is ticked.
      addVariant('ticked', '&:has(:checked)')
    }),
  ],
}

export default config

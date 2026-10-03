import { readFileSync } from 'node:fs'
import { join } from 'node:path'
import { describe, expect, it } from 'vitest'

type RGB = number[]
type Pair = [string, RGB, RGB, number]

const css = readFileSync(join(__dirname, 'app/globals.css'), 'utf8')
const darkAt = css.indexOf(":root[data-theme='dark']")
const darkEnd = css.indexOf('}', darkAt)

function colours(block: string): Record<string, RGB> {
  return Object.fromEntries(Array.from(block.matchAll(/--([\w-]+):\s*(\d+) (\d+) (\d+);/g), m => [m[1], [+m[2], +m[3], +m[4]]]))
}

function alphas(block: string): Record<string, number> {
  return Object.fromEntries(Array.from(block.matchAll(/--alpha-([\w-]+):\s*([\d.]+);/g), m => [m[1], +m[2]]))
}

const lightBlock = css.slice(0, darkAt)
const darkBlock = css.slice(darkAt, darkEnd)
const themes = {
  light: { c: colours(lightBlock), a: alphas(lightBlock) },
  dark: { c: { ...colours(lightBlock), ...colours(darkBlock) }, a: { ...alphas(lightBlock), ...alphas(darkBlock) } },
}

function luminance(rgb: RGB): number {
  const [r, g, b] = rgb.map(c => (c / 255 <= 0.04045 ? c / 255 / 12.92 : ((c / 255 + 0.055) / 1.055) ** 2.4))
  return 0.2126 * r + 0.7152 * g + 0.0722 * b
}

function contrast(a: RGB, b: RGB): number {
  const [hi, lo] = [luminance(a), luminance(b)].sort((x, y) => y - x)
  return (hi + 0.05) / (lo + 0.05)
}

const over = (fg: RGB, bg: RGB, alpha: number): RGB => fg.map((c, i) => c * alpha + bg[i] * (1 - alpha))

const SURFACES = ['background', 'surface', 'surface-inset', 'surface-raised']
const TEXT = ['text', 'text-muted', 'text-subtle', 'accent']
const TONES = ['success', 'warning', 'danger', 'info', 'accent', 'neutral']
const SERIES = [1, 2, 3, 4, 5, 6, 7, 8].map(n => `chart-${n}`)
const BODY = 4.5
const UI_PART = 3

const failures = (pairs: Pair[]) =>
  pairs.filter(([, fg, bg, min]) => contrast(fg, bg) < min).map(([name, fg, bg]) => `${name}: ${contrast(fg, bg).toFixed(2)}`)

describe('the theme', () => {
  it('has a dark block that sets every colour and every state the light one does', () => {
    expect(Object.keys(colours(darkBlock)).sort()).toEqual(Object.keys(colours(lightBlock)).sort())
    expect(Object.keys(alphas(darkBlock)).sort()).toEqual(Object.keys(alphas(lightBlock)).sort())
  })

  it('is light unless a choice says otherwise: no block follows the operating system', () => {
    expect(css).not.toMatch(/prefers-color-scheme/)
  })
})

describe.each(Object.entries(themes))('%s theme meets WCAG AA', (_, { c: t, a }) => {
  // The fill each state class of tailwind.config.ts lays over a surface.
  const hover = (bg: string) => over(t.text, t[bg], a.hover)
  const highlight = (bg: string) => over(t.text, t[bg], a.highlight)
  const selected = (bg: string) => over(t.accent, t[bg], a.selected)
  const toneTint = (tone: string, bg: string) => over(t[tone], t[bg], a.tint)
  const toneFaint = (tone: string, bg: string) => over(t[tone], t[bg], a.faint)

  it('text on every surface', () => {
    expect(failures(SURFACES.flatMap(bg => TEXT.map((fg): Pair => [`${fg} on ${bg}`, t[fg], t[bg], BODY])))).toEqual([])
  })

  it('text on a hovered, highlighted or selected row, on every surface a row sits on', () => {
    expect(failures(SURFACES.flatMap(bg => ['text', 'text-muted', 'text-subtle'].flatMap((fg): Pair[] => [
      [`${fg} on hover over ${bg}`, t[fg], hover(bg), BODY],
      [`${fg} on highlight over ${bg}`, t[fg], highlight(bg), BODY],
      [`${fg} on selected over ${bg}`, t[fg], selected(bg), BODY],
    ])))).toEqual([])
  })

  it('ink buttons and the selected segment', () => {
    expect(failures([
      ['primary-foreground on primary', t['primary-foreground'], t.primary, BODY],
      ['primary-foreground on primary-hover', t['primary-foreground'], t['primary-hover'], BODY],
    ])).toEqual([])
  })

  // A badge, an error banner and a danger button are the tone on its tint; a callout is text or
  // the tone on its faint fill. Each sits on a card, an inset panel or the page.
  it('each tone as text, on a surface, its tint and its faint fill', () => {
    expect(failures(TONES.flatMap(tone => SURFACES.flatMap((bg): Pair[] => [
      [`${tone} on ${bg}`, t[tone], t[bg], BODY],
      [`${tone} on its tint over ${bg}`, t[tone], toneTint(tone, bg), BODY],
      [`${tone} on its faint fill over ${bg}`, t[tone], toneFaint(tone, bg), BODY],
      [`text on the ${tone} faint fill over ${bg}`, t.text, toneFaint(tone, bg), BODY],
      [`text-muted on the ${tone} faint fill over ${bg}`, t['text-muted'], toneFaint(tone, bg), BODY],
      [`text on the ${tone} tint over ${bg}`, t.text, toneTint(tone, bg), BODY],
    ])))).toEqual([])
  })

  it('a badge on a highlighted scoreboard row: the tone on its tint over a faint tone fill', () => {
    expect(failures(TONES.flatMap(tone => ['danger', 'success'].map((row): Pair =>
      [`${tone} badge on a ${row} row`, t[tone], over(t[tone], toneFaint(row, 'surface'), a.tint), BODY],
    )))).toEqual([])
  })

  it('the edge of a control, the focus ring, a selection edge and a filled control against every surface', () => {
    expect(failures(SURFACES.flatMap(bg => ['border-control', 'ring', 'accent', 'primary', 'warning'].map((part): Pair => [`${part} against ${bg}`, t[part], t[bg], UI_PART])))).toEqual([])
  })

  it('the focus ring and the selection edge against a selected fill', () => {
    expect(failures(SURFACES.flatMap(bg => ['ring', 'accent'].map((part): Pair => [`${part} against selected over ${bg}`, t[part], selected(bg), UI_PART])))).toEqual([])
  })

  it('each chart series against the surfaces a chart sits on', () => {
    expect(failures(['surface', 'surface-raised'].flatMap(bg => SERIES.map((s): Pair => [`${s} against ${bg}`, t[s], t[bg], UI_PART])))).toEqual([])
  })

  // The graph canvas is surface; a node is inset, or raised when selected or hovered.
  it('the hypothesis graph: lines and labels on the canvas, statuses on a node', () => {
    expect(failures([
      ...['border-control', 'danger', 'info'].map((line): Pair => [`${line} link against the canvas`, t[line], t.surface, UI_PART]),
      ...['text-muted', 'danger', 'info'].map((label): Pair => [`${label} link label on the canvas`, t[label], t.surface, BODY]),
      ...['surface-inset', 'surface-raised'].flatMap(node => ['success', 'danger', 'info', 'neutral', 'text', 'text-muted'].map((fg): Pair => [`${fg} on a ${node} node`, t[fg], t[node], BODY])),
      ['chart tick text on a card', t['text-muted'], t.surface, BODY],
    ])).toEqual([])
  })
})

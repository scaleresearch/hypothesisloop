import { readFileSync } from 'node:fs'
import { join } from 'node:path'
import { describe, expect, it } from 'vitest'

type RGB = number[]

const css = readFileSync(join(__dirname, 'app/globals.css'), 'utf8')
const darkAt = css.indexOf('@media (prefers-color-scheme: dark)')

function tokens(block: string): Record<string, RGB> {
  return Object.fromEntries(Array.from(block.matchAll(/--([\w-]+):\s*(\d+) (\d+) (\d+);/g), m => [m[1], [+m[2], +m[3], +m[4]]]))
}

const light = tokens(css.slice(0, darkAt))
const schemes = { light, dark: { ...light, ...tokens(css.slice(darkAt)) } }

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
const TONES = ['success', 'warning', 'danger', 'info', 'accent', 'purple', 'pink', 'orange', 'neutral']
const BODY = 4.5
const UI_PART = 3

describe.each(Object.entries(schemes))('%s scheme meets WCAG AA', (_, t) => {
  const failures = (pairs: [string, RGB, RGB, number][]) =>
    pairs.filter(([, fg, bg, min]) => contrast(fg, bg) < min).map(([name, fg, bg]) => `${name}: ${contrast(fg, bg).toFixed(2)}`)

  it('text on every surface', () => {
    expect(failures(SURFACES.flatMap(bg => TEXT.map((fg): [string, RGB, RGB, number] => [`${fg} on ${bg}`, t[fg], t[bg], BODY])))).toEqual([])
  })

  it('text on the fills a hover, a selection and a focused option put under it', () => {
    expect(failures([
      ['text-muted on a hovered row', t['text-muted'], over(t.text, t.surface, 0.05), BODY],
      ['text-subtle on a hovered table row', t['text-subtle'], over(t.primary, t.surface, 0.05), BODY],
      ['text-subtle on a focused option', t['text-subtle'], over(t.primary, t['surface-raised'], 0.1), BODY],
      ['text-muted on a selected row', t['text-muted'], over(t.primary, t.surface, 0.15), BODY],
      ['primary-foreground on primary', t['primary-foreground'], t.primary, BODY],
      ['primary-foreground on primary-hover', t['primary-foreground'], t['primary-hover'], BODY],
    ])).toEqual([])
  })

  // A badge is the tone on 15% of itself, a hovered danger button on 20%, a callout the text on 10%.
  it('each tone as text, on a surface and on its own tint', () => {
    expect(failures(TONES.flatMap(tone => ['surface', 'surface-inset'].flatMap((bg): [string, RGB, RGB, number][] => [
      [`${tone} on ${bg}`, t[tone], t[bg], BODY],
      [`${tone} on its 20% tint over ${bg}`, t[tone], over(t[tone], t[bg], 0.2), BODY],
      [`text on the 10% ${tone} tint over ${bg}`, t.text, over(t[tone], t[bg], 0.1), BODY],
    ])))).toEqual([])
  })

  it('the edge of a control, the focus ring and a filled control against every surface', () => {
    expect(failures(SURFACES.flatMap(bg => ['border-control', 'ring', 'primary'].map((part): [string, RGB, RGB, number] => [`${part} against ${bg}`, t[part], t[bg], UI_PART])))).toEqual([])
  })
})

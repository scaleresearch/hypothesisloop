import { readFileSync } from 'node:fs'
import { join } from 'node:path'
import { describe, expect, it } from 'vitest'

type RGB = number[]

const css = readFileSync(join(__dirname, 'app/globals.css'), 'utf8')
const darkAt = css.indexOf(":root[data-theme='dark']")
const darkEnd = css.indexOf('}', darkAt)

function colours(block: string): Record<string, RGB> {
  return Object.fromEntries(Array.from(block.matchAll(/--([\w-]+):\s*(\d+) (\d+) (\d+);/g), m => [m[1], [+m[2], +m[3], +m[4]]]))
}

const lightBlock = css.slice(0, darkAt)
const darkBlock = css.slice(darkAt, darkEnd)
const themes = {
  light: colours(lightBlock),
  dark: { ...colours(lightBlock), ...colours(darkBlock) },
}

function luminance(rgb: RGB): number {
  const [r, g, b] = rgb.map(c => (c / 255 <= 0.04045 ? c / 255 / 12.92 : ((c / 255 + 0.055) / 1.055) ** 2.4))
  return 0.2126 * r + 0.7152 * g + 0.0722 * b
}

function contrast(a: RGB, b: RGB): number {
  const [hi, lo] = [luminance(a), luminance(b)].sort((x, y) => y - x)
  return (hi + 0.05) / (lo + 0.05)
}

const SURFACES = ['background', 'surface', 'surface-inset', 'surface-raised']
const TEXT = ['text', 'text-muted', 'text-subtle', 'accent']

describe.each(Object.entries(themes))('%s theme meets WCAG AA', (_, t) => {
  it('text on every surface', () => {
    const failures = SURFACES.flatMap(bg => TEXT.filter(fg => contrast(t[fg], t[bg]) < 4.5).map(fg => `${fg} on ${bg}`))
    expect(failures).toEqual([])
  })
})

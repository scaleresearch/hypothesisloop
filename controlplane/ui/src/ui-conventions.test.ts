import { readdirSync, readFileSync } from 'node:fs'
import { join, relative } from 'node:path'
import { describe, expect, it } from 'vitest'

const SRC = join(__dirname)
const UI = 'components/ui/'
const THEME = ['lib/colors.ts']

function sources(dir: string): string[] {
  return readdirSync(dir, { withFileTypes: true }).flatMap(entry => {
    const path = join(dir, entry.name)
    if (entry.isDirectory()) return sources(path)
    return /\.tsx?$/.test(entry.name) && !/\.test\.tsx?$/.test(entry.name) ? [relative(SRC, path)] : []
  })
}

function offences(files: string[], rules: Record<string, RegExp>): string[] {
  return files.flatMap(file =>
    readFileSync(join(SRC, file), 'utf8').split('\n').flatMap((line, i) =>
      Object.entries(rules).filter(([, pattern]) => pattern.test(line)).map(([rule]) => `${file}:${i + 1} ${rule}: ${line.trim()}`),
    ),
  )
}

const PALETTE = 'slate|gray|zinc|neutral|stone|red|orange|amber|yellow|lime|green|emerald|teal|cyan|sky|blue|indigo|violet|purple|fuchsia|pink|rose'
const COLOR_UTILITY = 'bg|text|border|ring|fill|stroke|from|to|via|divide|outline|shadow|decoration|placeholder|accent|caret'

const colours = {
  'Tailwind palette class, use a token class': new RegExp(`\\b(${COLOR_UTILITY})-((${PALETTE})-\\d{2,3}|white|black)\\b`),
  'hex colour, use a token': /#(?:[0-9a-fA-F]{8}|[0-9a-fA-F]{6}|[0-9a-fA-F]{3,4})\b/,
  'colour function, use a token': /\b(?:rgba?|hsla?)\(/,
  'opacity on a colour, use a state or tint token (bg-hover, bg-selected, bg-danger-tint, ...)': new RegExp(`\\b(${COLOR_UTILITY})-[a-z][\\w-]*/(\\d|\\[)`),
  'numbered opacity, use opacity-disabled or opacity-dimmed': /\bopacity-(?!0\b|100\b|disabled\b|dimmed\b)[\w[]/,
}

const controls = {
  'raw control, use the one in components/ui': /<(?:button|input|select|textarea|table)[\s>]/,
  'CSS variable, use a token class or lib/colors': /var\(--(?!font-)/,
}

const nativeSelect = {
  'native select, use Select or SearchableSelect': /<select[\s>]|\bSelect as \w+\b.*@headlessui/,
}

describe('UI conventions', () => {
  const files = sources(SRC)

  it('takes every colour from the theme tokens', () => {
    expect(offences(files.filter(f => !THEME.includes(f)), colours)).toEqual([])
  })

  it('styles controls only in components/ui', () => {
    expect(offences(files.filter(f => !f.startsWith(UI) && !THEME.includes(f)), controls)).toEqual([])
  })

  it('has no native select, the shared components included', () => {
    expect(offences(files, nativeSelect)).toEqual([])
  })
})

// The type and opacity scales, in one place for tailwind.config.ts and for the charts and the
// graph, which draw text and fades outside of class names.

/** Pixel sizes, so a chart can set an SVG font-size from the same step a class does. */
export const textPx = {
  '2xs': 11.5,
  xs: 12.5,
  sm: 13.5,
  base: 14.5,
  lg: 16,
  xl: 19,
  '2xl': 27,
  '3xl': 44,
} as const

type TextSize = keyof typeof textPx

// Line heights of xs and sm are whole pixels, so a control of either holds the height of field.ts.
// Display sizes close up as they grow, the way a set headline does.
export const fontSize: Record<TextSize, [string, { lineHeight: string; letterSpacing?: string }]> = {
  '2xs': [`${textPx['2xs']}px`, { lineHeight: '16px', letterSpacing: '0.01em' }],
  xs: [`${textPx.xs}px`, { lineHeight: '18px' }],
  sm: [`${textPx.sm}px`, { lineHeight: '20px' }],
  base: [`${textPx.base}px`, { lineHeight: '1.55' }],
  lg: [`${textPx.lg}px`, { lineHeight: '1.5' }],
  xl: [`${textPx.xl}px`, { lineHeight: '1.35', letterSpacing: '-0.01em' }],
  '2xl': [`${textPx['2xl']}px`, { lineHeight: '1.2', letterSpacing: '-0.02em' }],
  '3xl': [`${textPx['3xl']}px`, { lineHeight: '1.08', letterSpacing: '-0.03em' }],
}

/** Two weights: regular for reading and headings, medium to mark a name, a label or a choice. */
export const fontWeight = { normal: 400, medium: 500 } as const

export const opacity = {
  /** A control that cannot be used now. */
  disabled: 0.5,
  /** A thing set aside: filtered out, or not a choice in the current step. */
  dimmed: 0.25,
} as const

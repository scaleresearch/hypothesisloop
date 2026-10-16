'use client'

import type { ReactNode } from 'react'
import { ExpandButton, Expandable, useExpanded } from '@/components/ui/expandable'
import { Eyebrow } from '@/components/ui/text'

/** A chart that can fill the window, named by its label while it does. */
export function ExpandableChart({ label, children }: { label: string; children: ReactNode }) {
  return (
    <Expandable label={label} className="relative">
      <ChartTitle label={label} />
      <ExpandButton className="absolute right-0 top-0 z-10" />
      {children}
    </Expandable>
  )
}

function ChartTitle({ label }: { label: string }) {
  return useExpanded() ? <Eyebrow className="mb-2">{label}</Eyebrow> : null
}

/** Holds a chart at its height in the page, and grows it into the rest of the window while it fills it. */
export function ChartBox({ height, children }: { height: number; children: ReactNode }) {
  const expanded = useExpanded()
  return <div className={expanded ? 'min-h-0 flex-fill' : undefined} style={expanded ? undefined : { height }}>{children}</div>
}

'use client'

import { createContext, useContext, useEffect, useState } from 'react'
import type { ReactNode } from 'react'
import { Maximize2, Minimize2 } from 'lucide-react'
import { Button } from '@/components/ui/button'

const Expansion = createContext<{ expanded: boolean; setExpanded: (expanded: boolean) => void } | null>(null)

function useExpansion() {
  const expansion = useContext(Expansion)
  if (!expansion) throw new Error('used outside an Expandable')
  return expansion
}

/** Whether the Expandable around fills the window, for a chart that sizes itself to it. */
export function useExpanded() {
  return useExpansion().expanded
}

/**
 * Content that can fill the whole window: its ExpandButton opens and restores it, and Escape
 * restores it unless a layer above took the key first (with preventDefault).
 */
export function Expandable({ label, className, children }: {
  /** Names the content while it fills the window. */
  label: string
  /** Applies while it sits in the page. */
  className?: string
  children: ReactNode
}) {
  const [expanded, setExpanded] = useState(false)
  useEffect(() => {
    if (!expanded) return
    function onKey(e: KeyboardEvent) {
      if (e.key !== 'Escape' || e.defaultPrevented) return
      e.preventDefault()
      setExpanded(false)
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [expanded])

  return (
    <Expansion.Provider value={{ expanded, setExpanded }}>
      <div
        role={expanded ? 'region' : undefined}
        aria-label={expanded ? label : undefined}
        className={expanded ? 'fixed inset-0 z-40 flex flex-col bg-background p-4' : className}
      >
        {children}
      </div>
    </Expansion.Provider>
  )
}

export function ExpandButton({ className }: { className?: string }) {
  const { expanded, setExpanded } = useExpansion()
  const label = expanded ? 'Restore (Esc)' : 'Open in the whole window'
  return (
    <Button
      type="button"
      variant="ghost"
      size="icon"
      aria-label={label}
      title={label}
      onClick={() => setExpanded(!expanded)}
      className={className}
    >
      {expanded ? <Minimize2 size={13} /> : <Maximize2 size={13} />}
    </Button>
  )
}

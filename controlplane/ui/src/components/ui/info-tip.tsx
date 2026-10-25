'use client'

import { useLayoutEffect, useRef, useState } from 'react'
import { createPortal } from 'react-dom'
import { Info } from 'lucide-react'

const GAP = 6
const EDGE = 8

/**
 * The detail behind a one-line hint: shown at once on hover, on keyboard focus and on a tap, and
 * read by a screen reader as the icon's name. It floats over the page, so no card clips it.
 */
export function InfoTip({ children }: { children: string }) {
  const anchor = useRef<HTMLSpanElement>(null)
  const tip = useRef<HTMLDivElement>(null)
  const [open, setOpen] = useState(false)
  const [place, setPlace] = useState<{ left: number; top: number } | null>(null)

  useLayoutEffect(() => {
    if (!open) return setPlace(null)
    const a = anchor.current?.getBoundingClientRect()
    const t = tip.current?.getBoundingClientRect()
    if (!a || !t) return
    const left = Math.min(Math.max(a.left + a.width / 2 - t.width / 2, EDGE), window.innerWidth - t.width - EDGE)
    // Above the icon, or below it when the top of the window is too close.
    const top = a.top - t.height - GAP >= EDGE ? a.top - t.height - GAP : a.bottom + GAP
    setPlace({ left, top })
  }, [open])

  useLayoutEffect(() => {
    if (!open) return
    const close = () => setOpen(false)
    window.addEventListener('scroll', close, true)
    window.addEventListener('resize', close)
    return () => {
      window.removeEventListener('scroll', close, true)
      window.removeEventListener('resize', close)
    }
  }, [open])

  return (
    <span
      ref={anchor}
      role="img"
      aria-label={children}
      tabIndex={0}
      className="ml-1 inline-flex cursor-help rounded-full align-text-bottom text-subtle hover:text-muted focus-visible:text-muted"
      onMouseEnter={() => setOpen(true)}
      onMouseLeave={() => setOpen(false)}
      onFocus={() => setOpen(true)}
      onBlur={() => setOpen(false)}
      onKeyDown={e => { if (e.key === 'Escape') setOpen(false) }}
      // Inside a label a tap would otherwise move focus to the field instead of showing the tip.
      onClick={e => { e.preventDefault(); e.stopPropagation(); setOpen(true) }}
    >
      <Info size={13} aria-hidden="true" />
      {open && createPortal(
        <div
          ref={tip}
          aria-hidden="true"
          className="pointer-events-none fixed z-50 max-w-xs rounded border bg-surface-raised px-3 py-2 text-xs font-normal leading-normal text-foreground shadow-lg"
          style={place ?? { left: 0, top: 0, visibility: 'hidden' }}
        >
          {children}
        </div>,
        document.body,
      )}
    </span>
  )
}

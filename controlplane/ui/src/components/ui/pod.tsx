import type { CSSProperties, KeyboardEvent, ReactNode } from 'react'
import { clsx } from 'clsx'

interface PodProps {
  children: ReactNode
  className?: string
  style?: CSSProperties
  onClick?: () => void
}

/** Wolfram-style bordered panel — the base container used throughout the app. */
export function Pod({ children, className, style, onClick }: PodProps) {
  return (
    <div className={clsx('wa-pod', className)} style={style} onClick={onClick}>
      {children}
    </div>
  )
}

interface PodHeaderProps {
  children: ReactNode
  className?: string
  style?: CSSProperties
  onClick?: () => void
  tabIndex?: number
  role?: string
  onKeyDown?: (e: KeyboardEvent) => void
}

export function PodHeader({ children, className, style, onClick, tabIndex, role, onKeyDown }: PodHeaderProps) {
  return (
    <div
      className={clsx('wa-pod-header', className)}
      style={style}
      onClick={onClick}
      tabIndex={tabIndex}
      role={role}
      onKeyDown={onKeyDown}
    >
      {children}
    </div>
  )
}

interface PodContentProps {
  children: ReactNode
  className?: string
  style?: CSSProperties
  onClick?: () => void
  scrollX?: boolean
}

export function PodContent({ children, className, style, onClick, scrollX }: PodContentProps) {
  // scrollX is a modifier on the standard content box, not a different box: returning early here
  // dropped wa-pod-content along with every caller-supplied prop, so 15+ scrolling panels lost
  // the padding every other panel has.
  return (
    <div
      className={clsx('wa-pod-content', className)}
      style={scrollX ? { overflowX: 'auto', ...style } : style}
      onClick={onClick}
    >
      {children}
    </div>
  )
}

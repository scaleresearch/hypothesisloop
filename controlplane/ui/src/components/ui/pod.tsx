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
  if (scrollX) {
    return (
      <div style={{ overflowX: 'auto' }}>
        {children}
      </div>
    )
  }
  return (
    <div className={clsx('wa-pod-content', className)} style={style} onClick={onClick}>
      {children}
    </div>
  )
}

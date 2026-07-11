/** Shimmering skeleton bars — used wherever data is in flight instead of a bare "Loading…" string. */
export function Loading({ text = 'Loading…', rows = 3 }: { text?: string; rows?: number }) {
  return (
    <div className="skeleton-group" role="status" aria-label={text}>
      {Array.from({ length: rows }).map((_, i) => (
        <div key={i} className="skeleton-bar" style={{ width: i === rows - 1 ? '62%' : '100%' }} />
      ))}
    </div>
  )
}

export function ErrorMessage({ children }: { children: React.ReactNode }) {
  return (
    <div className="error-banner">
      <span className="error-banner-dot" />
      <span>{children}</span>
    </div>
  )
}

export function EmptyState({ children }: { children: React.ReactNode }) {
  return <p className="empty-state">{children}</p>
}

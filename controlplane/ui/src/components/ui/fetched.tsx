import type { ReactNode } from 'react'

interface FetchedProps<T> {
  data: T | undefined
  error: Error | undefined
  children: (data: T) => ReactNode
}

/**
 * A figure read from the API: itself once read, a quiet mark while it is in flight, and a marked
 * unknown carrying the reason when the read failed, so neither reads as a zero.
 */
export function Fetched<T>({ data, error, children }: FetchedProps<T>) {
  if (error) {
    return <span className="cursor-help text-danger" role="img" aria-label={`Unknown: ${error.message}`} title={`Could not load: ${error.message}`}>?</span>
  }
  if (data === undefined) return <span className="animate-pulse text-subtle" role="img" aria-label="Loading">…</span>
  return <>{children(data)}</>
}

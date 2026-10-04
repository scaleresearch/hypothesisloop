import { useEffect, useState } from 'react'

/** The whole seconds left until `at`, read again every second, at least 0; null without one. */
export function useSecondsUntil(at: string | undefined): number | null {
  const [now, setNow] = useState(() => Date.now())
  useEffect(() => {
    if (!at) return
    setNow(Date.now())
    const timer = setInterval(() => setNow(Date.now()), 1_000)
    return () => clearInterval(timer)
  }, [at])
  if (!at) return null
  return Math.max(0, Math.ceil((new Date(at).getTime() - now) / 1_000))
}

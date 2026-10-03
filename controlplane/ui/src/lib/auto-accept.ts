import { useCallback, useEffect, useRef, useState } from 'react'

export const AUTO_ACCEPT_SECONDS = 10

/**
 * Counts down to accepting a proposal by itself, for a person who watched it being planned and
 * has nothing to change. `arrived` says the page saw this proposal arrive: it was open while the
 * planner worked. One that is already there when the page loads never counts down. The countdown
 * starts once, on a visible page, when the component showing the proposal mounts. `stop` ends it
 * for good, as the page being hidden does, and at zero `accept` is called once.
 */
export function useAutoAccept(arrived: boolean, accept: () => void): { secondsLeft: number | null; stop: () => void } {
  const [secondsLeft, setSecondsLeft] = useState<number | null>(null)
  const latest = useRef(accept)
  latest.current = accept
  const onMount = useRef(arrived)

  useEffect(() => {
    if (onMount.current && document.visibilityState === 'visible') setSecondsLeft(AUTO_ACCEPT_SECONDS)
  }, [])

  const counting = secondsLeft !== null
  useEffect(() => {
    if (!counting) return
    const whenHidden = () => {
      if (document.visibilityState === 'hidden') setSecondsLeft(null)
    }
    document.addEventListener('visibilitychange', whenHidden)
    return () => document.removeEventListener('visibilitychange', whenHidden)
  }, [counting])

  useEffect(() => {
    if (secondsLeft === null) return
    if (secondsLeft === 0) {
      setSecondsLeft(null)
      latest.current()
      return
    }
    const timer = setTimeout(() => setSecondsLeft(secondsLeft - 1), 1_000)
    return () => clearTimeout(timer)
  }, [secondsLeft])

  const stop = useCallback(() => setSecondsLeft(null), [])
  return { secondsLeft, stop }
}

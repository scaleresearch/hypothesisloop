import { useCallback, useEffect, useRef, useState } from 'react'

export const AUTO_ACCEPT_SECONDS = 10

const seenKey = (draftID: string) => `hypothesisloop.proposal-seen.${draftID}`

/**
 * Counts down to accepting a proposal by itself, for a person who reads it and has nothing to
 * change. A proposal gets one countdown: when the page first shows it, visible, and never when
 * this browser showed it before, which localStorage remembers by its proposed_at. A revision
 * makes a new proposal with a countdown of its own. `stop` ends it for good, as the page being
 * hidden does, and at zero `accept` is called once. `proposedAt` is undefined while there is
 * nothing this person may accept.
 */
export function useAutoAccept(draftID: string, proposedAt: string | undefined, accept: () => void): { secondsLeft: number | null; stop: () => void } {
  const [secondsLeft, setSecondsLeft] = useState<number | null>(null)
  const latest = useRef(accept)
  latest.current = accept

  useEffect(() => {
    if (!proposedAt) return
    const key = seenKey(draftID)
    if (window.localStorage.getItem(key) === proposedAt) return
    window.localStorage.setItem(key, proposedAt)
    if (document.visibilityState === 'visible') setSecondsLeft(AUTO_ACCEPT_SECONDS)
  }, [draftID, proposedAt])

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

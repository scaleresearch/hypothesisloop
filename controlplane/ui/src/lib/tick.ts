import { useEffect, useState } from 'react'

// Rerenders on a timer, so an age on screen keeps up with the clock between reads of the API.
export function useTick(everyMs: number) {
  const [, tick] = useState(0)
  useEffect(() => {
    const timer = setInterval(() => tick(n => n + 1), everyMs)
    return () => clearInterval(timer)
  }, [everyMs])
}

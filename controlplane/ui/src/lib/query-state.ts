'use client'

import { useCallback } from 'react'
import { usePathname, useRouter, useSearchParams } from 'next/navigation'

// A filter/sort control backed by a URL query param instead of local state, so the current
// selection survives back/forward navigation and the URL can be bookmarked or shared.
// router.push (not replace) so each change is its own history entry. With replace, the URL is
// rewritten in place through the History API: no navigation, no server round trip and no history
// entry, for state that changes with every click, such as which item is selected.
export function useQueryParam(name: string, defaultValue = '', { replace = false } = {}) {
  const router = useRouter()
  const pathname = usePathname()
  const searchParams = useSearchParams()
  const value = searchParams.get(name) ?? defaultValue

  // Read from the location when called, so the setter stays the same function across renders.
  const setValue = useCallback((next: string) => {
    const params = new URLSearchParams(window.location.search)
    if (next && next !== defaultValue) params.set(name, next)
    else params.delete(name)
    const qs = params.toString()
    const url = qs ? `${pathname}?${qs}` : pathname
    if (replace) window.history.replaceState(window.history.state, '', url)
    else router.push(url)
  }, [name, defaultValue, replace, pathname, router])

  return [value, setValue] as const
}

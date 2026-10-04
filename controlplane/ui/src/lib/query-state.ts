'use client'

import { usePathname, useRouter, useSearchParams } from 'next/navigation'

// A filter/sort control backed by a URL query param instead of local state, so the current
// selection survives back/forward navigation and the URL can be bookmarked or shared.
// router.push (not replace) so each change is its own history entry.
export function useQueryParam(name: string, defaultValue = '') {
  const router = useRouter()
  const pathname = usePathname()
  const searchParams = useSearchParams()
  const value = searchParams.get(name) ?? defaultValue

  function setValue(next: string) {
    const params = new URLSearchParams(searchParams.toString())
    if (next && next !== defaultValue) params.set(name, next)
    else params.delete(name)
    const qs = params.toString()
    router.push(qs ? `${pathname}?${qs}` : pathname)
  }

  return [value, setValue] as const
}

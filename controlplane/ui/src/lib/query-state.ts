'use client'

import { useCallback, useEffect, useMemo } from 'react'
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

// The selected project is a query param like any other filter, and the one every page shares:
// in-app links and pushes carry it along, so moving between pages stays inside the project.
const PROJECT_PARAM = 'project'

function withProject(href: string, project: string): string {
  const [path, query = ''] = href.split('?')
  const params = new URLSearchParams(query)
  params.set(PROJECT_PARAM, project)
  return `${path}?${params.toString()}`
}

/** Maps an in-app href to the same place inside the selected project. */
export function useProjectHref(): (href: string) => string {
  const project = useSearchParams().get(PROJECT_PARAM)
  return useCallback((href: string) => (project ? withProject(href, project) : href), [project])
}

/** router.push and router.replace that stay inside the selected project. */
export function useProjectRouter() {
  const router = useRouter()
  const href = useProjectHref()
  return useMemo(() => ({
    push: (to: string) => router.push(href(to)),
    replace: (to: string) => router.replace(href(to)),
  }), [router, href])
}

/**
 * Puts the project a record belongs to in the URL once the record has loaded. A record is reached
 * by its id whatever project the link named, and the switcher, the nav links and the back link
 * all read the project from the URL.
 */
export function useRecordProject(projectID: string | undefined) {
  const router = useRouter()
  const pathname = usePathname()
  const searchParams = useSearchParams()
  const query = searchParams.toString()
  const current = searchParams.get(PROJECT_PARAM)
  useEffect(() => {
    if (projectID && projectID !== current) router.replace(withProject(`${pathname}?${query}`, projectID))
  }, [projectID, current, pathname, query, router])
}

/**
 * The project id the URL names, empty when it names none, and a way to switch. Switching lands on
 * the list page of the current section with its filters cleared: a record open in one project and
 * a filter on its experiments mean nothing in another.
 */
export function useProjectParam() {
  const router = useRouter()
  const pathname = usePathname()
  const value = useSearchParams().get(PROJECT_PARAM) ?? ''

  function select(projectID: string) {
    const section = `/${pathname.split('/')[1]}`
    router.push(withProject(section, projectID))
  }

  return [value, select] as const
}

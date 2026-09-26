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

// An example is a platform experiment of another org that the caller only reads. Its id rides
// the URL beside the project, on the pages that show one experiment's records.
const EXAMPLE_PARAM = 'example'
const EXAMPLE_PAGES = /^\/(platform-experiments\/[^/]+|jobs|hypotheses)(\/|$)/

function showsExample(href: string): boolean {
  return EXAMPLE_PAGES.test(href.split(/[?#]/)[0])
}

function withParams(href: string, set: Record<string, string>, drop: string[] = []): string {
  const [address, fragment] = href.split('#')
  const [path, query = ''] = address.split('?')
  const params = new URLSearchParams(query)
  for (const name of drop) params.delete(name)
  for (const [name, value] of Object.entries(set)) params.set(name, value)
  const qs = params.toString()
  return `${path}${qs ? `?${qs}` : ''}${fragment === undefined ? '' : `#${fragment}`}`
}

function withProject(href: string, project: string): string {
  return withParams(href, { [PROJECT_PARAM]: project })
}

/** The id of the example the URL is inside, empty on a page that shows none. */
export function useExampleParam(): string {
  const pathname = usePathname()
  const example = useSearchParams().get(EXAMPLE_PARAM) ?? ''
  return showsExample(pathname) ? example : ''
}

/** Where an example opens: its own page, with the example named in the URL. */
export function exampleHref(platformExperimentID: string): string {
  return withParams(`/platform-experiments/${platformExperimentID}`, { [EXAMPLE_PARAM]: platformExperimentID })
}

/**
 * Maps an in-app href to the same place inside the selected project and, between the pages of
 * an example, inside that example. `leaveExample` is for the links that lead out of it.
 */
export function useProjectHref(leaveExample = false): (href: string) => string {
  const searchParams = useSearchParams()
  const project = searchParams.get(PROJECT_PARAM)
  const example = searchParams.get(EXAMPLE_PARAM)
  return useCallback((href: string) => {
    const to = project ? withProject(href, project) : href
    return example && !leaveExample && showsExample(href) ? withParams(to, { [EXAMPLE_PARAM]: example }) : to
  }, [project, example, leaveExample])
}

/** router.push and router.replace that stay inside the selected project, and inside an example. */
export function useProjectRouter(leaveExample = false) {
  const router = useRouter()
  const href = useProjectHref(leaveExample)
  return useMemo(() => ({
    push: (to: string) => router.push(href(to)),
    replace: (to: string) => router.replace(href(to)),
  }), [router, href])
}

/**
 * Puts the project a record belongs to in the URL once the record has loaded. A record is reached
 * by its id whatever project the link named, and the switcher, the nav links and the back link
 * all read the project from the URL. A record of the caller's own is inside no example.
 */
export function useRecordProject(projectID: string | undefined) {
  const router = useRouter()
  const pathname = usePathname()
  const searchParams = useSearchParams()
  const query = searchParams.toString()
  const current = searchParams.get(PROJECT_PARAM)
  const example = searchParams.has(EXAMPLE_PARAM)
  useEffect(() => {
    if (!projectID || (projectID === current && !example)) return
    router.replace(withParams(`${pathname}?${query}`, { [PROJECT_PARAM]: projectID }, [EXAMPLE_PARAM]))
  }, [projectID, current, example, pathname, query, router])
}

/**
 * Puts the example a record belongs to in the URL once the record has loaded, for a record
 * reached by its id alone. The project stays the caller's own: the record's is not theirs.
 */
export function useRecordExample(platformExperimentID: string | undefined) {
  const router = useRouter()
  const pathname = usePathname()
  const searchParams = useSearchParams()
  const query = searchParams.toString()
  const current = searchParams.get(EXAMPLE_PARAM)
  useEffect(() => {
    if (!platformExperimentID || platformExperimentID === current) return
    router.replace(withParams(`${pathname}?${query}`, { [EXAMPLE_PARAM]: platformExperimentID }))
  }, [platformExperimentID, current, pathname, query, router])
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

'use client'

import { useEffect } from 'react'
import { useRouter, useSearchParams } from 'next/navigation'
import { connectGitHubInstallation, fetchGitHubApp, fetchGitHubInstallations } from '@/lib/api'
import { githubCardPath, leaveFor, redirectURI, takeReturn } from '@/lib/github-connect'
import { useTenant } from '@/lib/tenant'
import { Loading } from '@/components/ui/status-message'

/**
 * Where GitHub returns the person from installing or authorising the platform's App. The path is
 * the one registered on the App. The return is taken once, the connect it carries is made while
 * this page shows, and the person goes on to GitHub to install the App or back to the page they
 * left from, which shows how it went. A return no attempt of this tab accounts for lands on the
 * GitHub card of the org's settings.
 */
export default function GitHubReturnPage() {
  const { org } = useTenant()
  const router = useRouter()
  const query = useSearchParams().toString()

  useEffect(() => {
    void takeReturn(query, {
      app: fetchGitHubApp,
      connect: (orgID, code, installationID) => connectGitHubInstallation(orgID, { code, redirectURI: redirectURI(), installationID }),
      installations: async orgID => (await fetchGitHubInstallations(orgID)).items,
    }).then(next => {
      if (next.to === 'github') leaveFor(next.address)
      else router.replace(next.path ?? githubCardPath(org.id))
    })
  }, [query, org.id, router])

  return <Loading text="Returning from GitHub…" />
}

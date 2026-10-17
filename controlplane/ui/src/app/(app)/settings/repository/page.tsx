'use client'

import { useEffect, useState } from 'react'
import { useSearchParams } from 'next/navigation'
import { finishGitHubInstall } from '@/lib/api/github'
import { githubCardPath, takeReturn, type ReturnView } from '@/lib/github-connect'
import { useTenant } from '@/lib/tenant'
import { TextLink } from '@/components/ui/link'
import { Stack } from '@/components/ui/stack'
import { ErrorMessage, Loading } from '@/components/ui/status-message'
import { Text } from '@/components/ui/text'

/**
 * The App's callback URL: where GitHub returns the tab the install was opened in. The return is
 * taken once, the installation connected, the tab the install started from told, and this tab
 * closes. A tab the browser does not let close, as one opened by hand, says how it went and leads
 * back.
 */
export default function GitHubReturnPage() {
  const { org } = useTenant()
  const query = useSearchParams().toString()
  const [view, setView] = useState<ReturnView | null>(null)

  useEffect(() => {
    void takeReturn(query, finishGitHubInstall).then(taken => {
      setView(taken)
      if (taken.view !== 'nothing') window.close()
    })
  }, [query])

  if (!view) return <Loading text="Connecting GitHub…" />
  const back = view.view === 'connected' && view.returnTo ? view.returnTo : githubCardPath(org.id)
  return (
    <Stack start>
      {view.view === 'connected' && <Text>Connected {view.account}.</Text>}
      {view.view === 'failed' && <ErrorMessage>{view.message}</ErrorMessage>}
      {view.view === 'changed' && <Text>Your changes on GitHub are saved.</Text>}
      {view.view === 'nothing' && <Text>This page finishes an install of the GitHub App. Nothing was connected.</Text>}
      <TextLink href={back}>Back to HypothesisLoop</TextLink>
    </Stack>
  )
}

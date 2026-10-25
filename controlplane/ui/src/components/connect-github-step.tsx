'use client'

import { useEffect, useRef } from 'react'
import { clsx } from 'clsx'
import useSWR from 'swr'
import { Github } from 'lucide-react'
import { fetchGitHubInstallations, GITHUB_NOT_CONFIGURED } from '@/lib/api/github'
import { ApiError } from '@/lib/api/request'
import { useGitHubApp, useGitHubInstall } from '@/lib/github-connect'
import { useTenant } from '@/lib/tenant'
import { InstallProgress } from '@/components/github-status'
import { Button } from '@/components/ui/button'
import { Dialog } from '@/components/ui/dialog'
import { controlHeight } from '@/components/ui/field'
import { InlineError } from '@/components/ui/status-message'
import { Text } from '@/components/ui/text'
import { keys } from '@/lib/keys'
import type { OrgGitHubInstallation } from '@/types/api'

/** What a member who cannot connect GitHub is told. */
export const ASK_OWNER = 'Ask an owner of your organization to connect GitHub.'

/** The GitHub accounts the org connected, read under the key every GitHub view shares. */
function useInstallations() {
  const { org } = useTenant()
  const app = useGitHubApp()
  return useSWR(app.data ? keys.githubInstallations(org.id) : null, () => fetchGitHubInstallations(org.id))
}

/**
 * Where the org stands with GitHub when it creates an experiment: an account connected, to name a
 * repository on or have one created on, or none yet. Undefined while it is being read, and on a
 * deployment whose git host is another one, where the repository is named by hand.
 */
export function useGitHub(): 'connected' | 'unconnected' | undefined {
  const app = useGitHubApp()
  const installations = useInstallations()
  if (!app.data || !installations.data) return undefined
  return installations.data.total > 0 ? 'connected' : 'unconnected'
}

/** The GitHub accounts the org connected, once read. */
export function useGitHubAccounts(): OrgGitHubInstallation[] | undefined {
  return useInstallations().data?.items
}

/** Whether the deployment's git host is a plain one, where a repository is named by hand. */
export function usePlainGitHost(): boolean {
  const app = useGitHubApp()
  return app.error instanceof ApiError && app.error.reason === GITHUB_NOT_CONFIGURED
}

/** Connects GitHub from this page, in a new tab it waits for. */
export function useConnectGitHub() {
  const { org } = useTenant()
  return useGitHubInstall(org.id, typeof window === 'undefined' ? '/' : window.location.pathname + window.location.search)
}

/**
 * Asks for GitHub where an experiment needs its repository and none can be made yet. Connecting
 * runs in a new tab; once the org's accounts read as connected, `onConnected` is told and the
 * caller carries on. A member is told to ask an owner.
 */
export function ConnectGitHubDialog({ open, onClose, onConnected }: { open: boolean; onClose: () => void; onConnected: () => void }) {
  const { org } = useTenant()
  const owner = org.actions.includes('edit')
  const github = useGitHub()
  const { install, start, reset } = useConnectGitHub()
  const told = useRef(onConnected)
  told.current = () => { reset(); onConnected() }

  // Told once per opening, once a connect made here lands, or once the org reads as connected
  // after it did not: an org whose old account was removed opens this already counted as connected.
  const sawUnconnected = useRef(false)
  const done = useRef(false)
  useEffect(() => {
    if (!open) {
      sawUnconnected.current = false
      done.current = false
      return
    }
    if (github === 'unconnected') sawUnconnected.current = true
    if (!done.current && github === 'connected' && (sawUnconnected.current || install.phase === 'connected')) {
      done.current = true
      told.current()
    }
  }, [open, github, install.phase])

  const close = () => { reset(); onClose() }
  return (
    <Dialog open={open} onClose={close} title="Connect GitHub" size="sm">
      <Text tone="muted">{owner ? 'Your experiment gets its own private repository. Connect GitHub so we can create it for you.' : ASK_OWNER}</Text>
      {install.phase === 'waiting' && <Text variant="hint" role="status" className="mt-3">Finish connecting in the GitHub tab.</Text>}
      {install.phase === 'failed' && <InlineError className="mt-3 block">{install.message}</InlineError>}
      <div className="mt-5 flex justify-end gap-2">
        <Button type="button" onClick={close}>{owner ? 'Cancel' : 'Close'}</Button>
        {owner && (
          <Button type="button" variant="primary" loading={install.phase === 'waiting'} onClick={start}>
            <Github aria-hidden className="size-4" />
            Connect GitHub
          </Button>
        )}
      </div>
    </Dialog>
  )
}

/**
 * Stands in the repository field while the org has no GitHub account: one row of a field's
 * height with the way to connect. `invalid` marks it as what stops the create.
 */
export function ConnectGitHubRow({ invalid, describedBy }: { invalid?: boolean; describedBy?: string }) {
  const { org } = useTenant()
  const owner = org.actions.includes('edit')
  const { install, start, reset } = useConnectGitHub()
  return (
    <>
      <div
        data-invalid={invalid ? '' : undefined}
        className={clsx(
          controlHeight.default,
          'flex items-center justify-between gap-3 rounded border border-dashed border-border-control px-3',
          invalid && 'border-solid border-danger',
        )}
      >
        <span className="flex min-w-0 items-center gap-2 text-sm text-muted">
          <Github aria-hidden className="size-4 flex-none" />
          <span className="truncate">{owner ? 'Connect GitHub to create or choose a repository' : ASK_OWNER}</span>
        </span>
        {owner && (
          <Button type="button" size="sm" loading={install.phase === 'waiting'} aria-describedby={describedBy} onClick={start}>Connect GitHub</Button>
        )}
      </div>
      {install.phase !== 'idle' && <InstallProgress install={install} onCancel={reset} />}
    </>
  )
}

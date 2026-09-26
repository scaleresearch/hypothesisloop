'use client'

import { useState } from 'react'
import { fetchCodeBundle, platformCodeRef } from '@/lib/api'
import { Button } from '@/components/ui/button'
import { InlineError } from '@/components/ui/status-message'

/**
 * A job's code_ref, and for code the platform keeps (an experiment with no repository) the way
 * to take it: the commit as a git bundle, which `git clone` opens.
 */
export function CodeRef({ codeRef }: { codeRef: string }) {
  const kept = platformCodeRef(codeRef)
  const [state, setState] = useState<{ busy: boolean; error?: string }>({ busy: false })
  if (!kept) return <>{codeRef}</>

  async function download() {
    setState({ busy: true })
    try {
      const url = URL.createObjectURL(await fetchCodeBundle(codeRef))
      const link = document.createElement('a')
      link.href = url
      link.download = `${kept!.sha}.bundle`
      link.click()
      URL.revokeObjectURL(url)
      setState({ busy: false })
    } catch (e) {
      setState({ busy: false, error: e instanceof Error ? e.message : String(e) })
    }
  }

  return (
    <div className="grid justify-items-start gap-1">
      <span>{kept.sha}</span>
      <span className="font-sans text-xs text-muted">
        Kept by the platform: this experiment has no repository.{' '}
        <Button type="button" variant="link" size="sm" loading={state.busy} onClick={download}>Download as a git bundle</Button>
        {' '}and open it with <span className="font-mono">git clone {kept.sha.slice(0, 12)}….bundle</span>.
      </span>
      {state.error && <InlineError>Could not download the code: {state.error}</InlineError>}
    </div>
  )
}

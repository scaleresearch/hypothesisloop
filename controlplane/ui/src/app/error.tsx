'use client'

import { DeadEnd } from '@/components/dead-end'
import { Button } from '@/components/ui/button'
import { ErrorMessage } from '@/components/ui/status-message'

export default function PageError({ error, reset }: { error: Error & { digest?: string }; reset: () => void }) {
  return (
    <DeadEnd title="This page failed" actions={<Button onClick={reset}>Try again</Button>}>
      <ErrorMessage>
        {error.message}
        {error.digest && <> (reference {error.digest})</>}
      </ErrorMessage>
    </DeadEnd>
  )
}

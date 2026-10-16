'use client'

import './globals.css'
import { DeadEnd } from '@/components/dead-end'
import { Button } from '@/components/ui/button'
import { ErrorMessage } from '@/components/ui/status-message'

// Stands in for the root layout when the layout itself failed, so it brings its own document.
export default function GlobalError({ error, reset }: { error: Error & { digest?: string }; reset: () => void }) {
  return (
    <html lang="en">
      <body className="flex min-h-screen antialiased">
        <DeadEnd title="HypothesisLoop failed to load" actions={<Button onClick={reset}>Try again</Button>}>
          <ErrorMessage>
            {error.message}
            {error.digest && <> (reference {error.digest})</>}
          </ErrorMessage>
        </DeadEnd>
      </body>
    </html>
  )
}

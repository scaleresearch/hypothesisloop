'use client'

import { useExample } from '@/lib/example'
import { Link } from '@/components/ui/link'
import { Eyebrow } from '@/components/ui/text'

/** Says, on every page of an example, that it is one and that nothing in it can be changed. */
export function ExampleBanner() {
  const example = useExample()
  if (!example) return null
  return (
    <div role="note" className="mb-4 flex flex-wrap items-baseline gap-x-2.5 gap-y-1 rounded border bg-surface-inset px-3.5 py-2 text-xs text-muted">
      <Eyebrow>Example, read only</Eyebrow>
      {example.pe && <span className="min-w-0 truncate">{example.pe.name}</span>}
      <Link href="/examples" className="ml-auto whitespace-nowrap text-accent hover:underline">All examples</Link>
    </div>
  )
}

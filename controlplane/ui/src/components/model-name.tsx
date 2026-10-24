import { clsx } from 'clsx'
import { parseModelRef, PROVIDERS } from '@/lib/models'
import { Code } from '@/components/ui/text'

/** A model reference as its model id and a badge of its provider. */
export function ModelName({ model, className }: { model: string; className?: string }) {
  const { provider, id } = parseModelRef(model)
  const short = PROVIDERS.find(p => p.provider === provider)?.short
  return (
    <span title={model} className={clsx('inline-flex min-w-0 items-baseline gap-1.5', className)}>
      <Code className="truncate">{id}</Code>
      {short && <span className="flex-none rounded-full border px-1.5 text-2xs text-muted">{short}</span>}
    </span>
  )
}

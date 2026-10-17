'use client'

import { useRef } from 'react'
import { clsx } from 'clsx'
import { Check } from 'lucide-react'
import { PLACEHOLDER_DESCRIPTIONS, placeholdersIn } from '@/lib/agent-types'
import { Textarea } from '@/components/ui/input'
import { Stack } from '@/components/ui/stack'
import { Eyebrow, Text } from '@/components/ui/text'

function kb(n: number): string {
  return `${(n / 1024).toFixed(1)} KB`
}

/** A system prompt with the placeholders the API says its type may hold, each insertable. */
export function PromptEditor({ placeholders, required, value, onChange, readOnly }: {
  placeholders: string[]
  required: string[]
  value: string
  onChange?: (prompt: string) => void
  readOnly?: boolean
}) {
  const area = useRef<HTMLTextAreaElement>(null)
  const present = placeholdersIn(value)
  const size = new TextEncoder().encode(value).length

  function insert(name: string) {
    const el = area.current
    const token = `{{${name}}}`
    const at = el?.selectionStart ?? value.length
    const end = el?.selectionEnd ?? at
    onChange?.(value.slice(0, at) + token + value.slice(end))
    requestAnimationFrame(() => {
      el?.focus()
      el?.setSelectionRange(at + token.length, at + token.length)
    })
  }

  return (
    <Stack gap="sm">
      <Stack gap="xs" role="group" aria-label="Placeholders">
        <Eyebrow>Placeholders{!readOnly && <span className="font-normal text-subtle"> · click one to insert it at the cursor</span>}</Eyebrow>
        <div className="grid grid-cols-1 gap-x-6 gap-y-1.5 md:grid-cols-2">
          {placeholders.map(name => {
            const needed = required.includes(name)
            const used = present.includes(name)
            return (
              <div key={name} className="flex min-w-0 items-baseline gap-2">
                <button
                  type="button"
                  disabled={readOnly}
                  onClick={() => insert(name)}
                  title={PLACEHOLDER_DESCRIPTIONS[name]}
                  aria-label={`Insert {{${name}}}`}
                  className={clsx(
                    'inline-flex flex-none items-center gap-1 rounded-full border px-2 py-0.5 font-mono text-2xs transition-colors',
                    used ? 'border-success-edge bg-success-tint text-success' : needed ? 'border-danger-edge bg-danger-tint text-danger' : 'bg-surface-inset text-muted',
                    !readOnly && 'hover:border-border-strong',
                  )}
                >
                  {used && <Check aria-hidden="true" size={10} />}
                  {`{{${name}}}`}
                </button>
                <Text as="span" variant="hint" className="min-w-0">
                  {PLACEHOLDER_DESCRIPTIONS[name]}{needed && <span className="font-medium text-muted"> Required.</span>}
                </Text>
              </div>
            )
          })}
        </div>
      </Stack>
      <Textarea
        ref={area}
        aria-label="Prompt"
        mono
        spellCheck={false}
        rows={readOnly ? 24 : 28}
        readOnly={readOnly}
        value={value}
        onChange={e => onChange?.(e.target.value)}
        className="text-xs leading-relaxed"
      />
      <Text variant="meta" className="text-right tabular-nums">{kb(size)}</Text>
    </Stack>
  )
}

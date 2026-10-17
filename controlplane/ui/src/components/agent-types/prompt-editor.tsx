'use client'

import { useRef } from 'react'
import { clsx } from 'clsx'
import { Check } from 'lucide-react'
import { MAX_PROMPT_BYTES, REQUIRED_PLACEHOLDERS, placeholdersIn, placeholdersOf, promptProblems } from '@/lib/agent-types'
import { Textarea } from '@/components/ui/input'
import { Stack } from '@/components/ui/stack'
import { InlineError } from '@/components/ui/status-message'
import { Eyebrow, Text } from '@/components/ui/text'
import type { AgentRole } from '@/types/api'

function kb(n: number): string {
  return `${(n / 1024).toFixed(1)} KB`
}

/** A system prompt with the placeholders its role may hold, each insertable, and what the API would refuse in it. */
export function PromptEditor({ role, value, onChange, readOnly }: {
  role: AgentRole
  value: string
  onChange?: (prompt: string) => void
  readOnly?: boolean
}) {
  const area = useRef<HTMLTextAreaElement>(null)
  const present = placeholdersIn(value)
  const problems = readOnly ? [] : promptProblems(role, value)
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
          {placeholdersOf(role).map(p => {
            const required = REQUIRED_PLACEHOLDERS.includes(p.name)
            const used = present.includes(p.name)
            return (
              <div key={p.name} className="flex min-w-0 items-baseline gap-2">
                <button
                  type="button"
                  disabled={readOnly}
                  onClick={() => insert(p.name)}
                  title={`${p.description} Filled by the ${p.filledBy}.`}
                  aria-label={`Insert {{${p.name}}}`}
                  className={clsx(
                    'inline-flex flex-none items-center gap-1 rounded-full border px-2 py-0.5 font-mono text-2xs transition-colors',
                    used ? 'border-success-edge bg-success-tint text-success' : required ? 'border-danger-edge bg-danger-tint text-danger' : 'bg-surface-inset text-muted',
                    !readOnly && 'hover:border-border-strong',
                  )}
                >
                  {used && <Check aria-hidden="true" size={10} />}
                  {`{{${p.name}}}`}
                </button>
                <Text as="span" variant="hint" className="min-w-0">
                  {p.description}{required && <span className="font-medium text-muted"> Required.</span>}
                </Text>
              </div>
            )
          })}
        </div>
      </Stack>
      {problems.length > 0 && (
        <Stack gap="2xs" aria-live="polite" className="rounded border border-danger-edge bg-danger-faint px-3 py-2">
          {problems.map(p => <InlineError key={p}>{p}</InlineError>)}
        </Stack>
      )}
      <Textarea
        ref={area}
        aria-label="Prompt"
        mono
        spellCheck={false}
        rows={readOnly ? 24 : 28}
        readOnly={readOnly}
        invalid={problems.length > 0}
        value={value}
        onChange={e => onChange?.(e.target.value)}
        className="text-xs leading-relaxed"
      />
      <Text variant="meta" className="text-right tabular-nums">{kb(size)} of {kb(MAX_PROMPT_BYTES)}</Text>
    </Stack>
  )
}

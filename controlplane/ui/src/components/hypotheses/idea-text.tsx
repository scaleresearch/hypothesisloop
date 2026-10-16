'use client'

import { useEffect, useState } from 'react'
import { editHumanHypothesis, removeHumanHypothesis } from '@/lib/api/hypotheses'
import type { HypothesisGraphNode } from '@/types/hypotheses'
import { Button } from '@/components/ui/button'
import { Textarea } from '@/components/ui/input'
import { Inline, Stack } from '@/components/ui/stack'
import { InlineError } from '@/components/ui/status-message'
import { Text } from '@/components/ui/text'
import { useTenant } from '@/lib/tenant'
import { useEditor } from '@/components/hypotheses/editor'

/**
 * The text of an idea, and while the signed-in person may still change it, their way to edit it
 * inline (its author) or remove it (its author or an owner of its org). Ideas an agent has acted on,
 * through a job or a link to it, are part of the record and are left as they are.
 */
export function IdeaText({ node }: { node: HypothesisGraphNode }) {
  const editor = useEditor()
  const { me, org } = useTenant()
  const [mode, setMode] = useState<'read' | 'edit' | 'remove'>('read')
  const [text, setText] = useState(node.text)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  useEffect(() => {
    setMode('read')
    setError('')
  }, [node.id])

  const persons = !node.agent_id && node.actor.kind === 'user'
  const untouched = !node.best_job && node.finding_count === 0 && !editor?.linked.has(node.id)
  const open = !!editor?.canWrite && persons && untouched
  const author = open && !!me.user_id && node.actor.id === me.user_id
  const remover = author || (open && org.role === 'owner')

  async function run(change: () => Promise<unknown>) {
    setBusy(true)
    setError('')
    try {
      await change()
      setMode('read')
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
    } finally {
      setBusy(false)
    }
  }

  return (
    <Stack gap="sm">
      {mode === 'edit' ? (
        <Textarea className="min-h-24 w-full" value={text} onChange={e => setText(e.target.value)} aria-label="Text of the idea" autoFocus />
      ) : (
        <Text pre className="max-h-60 overflow-y-auto leading-relaxed">{node.text}</Text>
      )}
      {mode === 'edit' && (
        <Inline gap="xs">
          <Button size="sm" variant="primary" loading={busy} disabled={text.trim() === '' || text.trim() === node.text}
            onClick={() => run(async () => {
              await editHumanHypothesis(node.id, text.trim())
              await editor!.refresh()
            })}
          >
            Save
          </Button>
          <Button variant="link" size="sm" onClick={() => setMode('read')}>Cancel</Button>
        </Inline>
      )}
      {mode === 'remove' && (
        <Inline gap="xs">
          <span className="text-xs">Remove this idea and its comments?</span>
          <Button size="sm" variant="danger" loading={busy}
            onClick={() => run(async () => {
              await removeHumanHypothesis(node.id)
              editor!.select('')
              await editor!.refresh()
            })}
          >
            Remove
          </Button>
          <Button variant="link" size="sm" onClick={() => setMode('read')}>Keep it</Button>
        </Inline>
      )}
      {mode === 'read' && (author || remover) && (
        <Inline gap="xs">
          {author && <Button size="sm" onClick={() => { setText(node.text); setError(''); setMode('edit') }}>Edit</Button>}
          {remover && <Button size="sm" variant="danger" onClick={() => { setError(''); setMode('remove') }}>Remove</Button>}
        </Inline>
      )}
      {error && <InlineError>{error}</InlineError>}
    </Stack>
  )
}

'use client'

import { useEffect, useState } from 'react'
import { editHumanHypothesis, removeHumanHypothesis } from '@/lib/api/hypotheses'
import { Button } from '@/components/ui/button'
import { Textarea } from '@/components/ui/input'
import { Inline, Stack } from '@/components/ui/stack'
import { InlineError } from '@/components/ui/status-message'
import { Text } from '@/components/ui/text'
import { useEditor } from '@/components/hypotheses/editor'
import type { GraphNode } from '@/types/api'

/**
 * The text of an idea, and the ways to edit it inline or remove it that the API lists for the
 * caller: a person's own idea, or for an owner of its org any person's, while no agent acted on it.
 */
export function IdeaText({ node }: { node: GraphNode }) {
  const editor = useEditor()
  const [mode, setMode] = useState<'read' | 'edit' | 'remove'>('read')
  const [text, setText] = useState(node.text)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  useEffect(() => {
    setMode('read')
    setError('')
  }, [node.id])

  const edits = node.actions.includes('edit')
  const removes = node.actions.includes('remove')

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
      {mode === 'read' && (edits || removes) && (
        <Inline gap="xs">
          {edits && <Button size="sm" onClick={() => { setText(node.text); setError(''); setMode('edit') }}>Edit</Button>}
          {removes && <Button size="sm" variant="danger" onClick={() => { setError(''); setMode('remove') }}>Remove</Button>}
        </Inline>
      )}
      {error && <InlineError>{error}</InlineError>}
    </Stack>
  )
}

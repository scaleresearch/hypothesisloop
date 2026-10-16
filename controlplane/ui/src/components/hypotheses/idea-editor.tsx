'use client'

import { useState } from 'react'
import { X } from 'lucide-react'
import { registerHumanHypothesis } from '@/lib/api/hypotheses'
import { Button, Chip } from '@/components/ui/button'
import { Textarea } from '@/components/ui/input'
import { Inline, Stack } from '@/components/ui/stack'
import { InlineError } from '@/components/ui/status-message'
import { Eyebrow, Text } from '@/components/ui/text'
import { LINK_KINDS, LINK_KIND_HINTS, LINK_KIND_LABELS } from '@/lib/hypothesis-graph'
import { IdeaSearch } from '@/components/hypotheses/editor-controls'
import { nodeLabel, useEditor } from '@/components/hypotheses/editor'

/**
 * Writing a new idea, the same beside the graph and the outline: its text and its links, builds_on
 * to the node it was opened for to start with.
 */
export function IdeaEditor() {
  const editor = useEditor()
  const [text, setText] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const [existingID, setExistingID] = useState('')
  if (!editor?.composing) return null
  const ed = editor

  async function submit() {
    setBusy(true)
    setError('')
    setExistingID('')
    try {
      const h = await registerHumanHypothesis(ed.graph.root, text.trim(), ed.links)
      if (h.already_existed) {
        setExistingID(h.id)
      } else {
        setText('')
        await ed.added(h)
      }
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
    } finally {
      setBusy(false)
    }
  }

  const buildsOn = ed.links.some(l => l.type === 'builds_on')

  return (
    <Stack gap="sm" className="rounded-lg border border-primary bg-surface-raised p-3" role="group" aria-label="New idea">
      <Eyebrow>New idea</Eyebrow>
      <Textarea
        className="min-h-16 w-full overflow-x-hidden whitespace-pre-wrap break-words"
        rows={3}
        value={text}
        onChange={e => setText(e.target.value)}
        placeholder="What to try, and why you expect it to help"
        aria-label="Your idea"
        autoFocus
      />
      <Stack gap="xs" className="text-xs">
        <Eyebrow>Links</Eyebrow>
        {ed.links.length > 0 && (
          <Stack as="ul" gap="sm" className="list-none">
            {ed.links.map(l => {
              const objective = l.to === ed.graph.root
              const label = objective ? `A new approach to: ${ed.rootLabel}` : nodeLabel(ed, l.to)
              return (
                <Stack as="li" gap="2xs" key={l.to} aria-label={`Link to ${nodeLabel(ed, l.to)}`}>
                  <Inline gap="xs" nowrap>
                    <span className="line-clamp-2 min-w-0 flex-1" title={label}>{label}</span>
                    <Button variant="ghost" size="icon" aria-label={`Remove link to ${nodeLabel(ed, l.to)}`} onClick={() => ed.removeLink(l.to)}><X size={12} /></Button>
                  </Inline>
                  {!objective && (
                    <>
                      <Inline gap="xs" role="group" aria-label="Mine">
                        {LINK_KINDS.map(k => (
                          <Chip key={k} active={l.type === k} title={LINK_KIND_HINTS[k]} onClick={() => ed.setLinkType(l.to, k)}>{LINK_KIND_LABELS[k]}</Chip>
                        ))}
                      </Inline>
                      <span className="text-subtle">Mine {LINK_KIND_HINTS[l.type]}.</span>
                    </>
                  )}
                </Stack>
              )
            })}
          </Stack>
        )}
        {ed.findingTarget ? (
          <Stack gap="xs">
            <IdeaSearch />
            <Inline>
              <span className="text-subtle">or click it in the view.</span>
              <Button variant="link" size="sm" className="ml-auto" onClick={() => ed.setFindingTarget(false)}>Stop</Button>
            </Inline>
          </Stack>
        ) : (
          <div>
            <Button variant="link" size="sm" onClick={() => ed.setFindingTarget(true)}>+ Add a link</Button>
          </div>
        )}
        {!buildsOn && <Text variant="hint">It needs a link to the objective or to an idea it refines.</Text>}
      </Stack>
      {existingID && (
        <Text as="span" variant="note">
          An equivalent idea already exists, so nothing was added.{' '}
          <Button variant="link" size="sm" onClick={() => ed.show(existingID)}>Show it</Button>
        </Text>
      )}
      {error && <InlineError>{error}</InlineError>}
      <Inline>
        <Button size="sm" variant="primary" loading={busy} disabled={text.trim() === '' || !buildsOn} onClick={submit}>
          Add idea
        </Button>
        <Button variant="link" size="sm" className="ml-auto" onClick={ed.cancel}>Cancel</Button>
      </Inline>
    </Stack>
  )
}

'use client'

import { useState } from 'react'
import type { CSSProperties } from 'react'
import { Plus, X } from 'lucide-react'
import { clsx } from 'clsx'
import { Badge } from '@/components/ui/badge'
import { Button, Chip, RowButton } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Inline, Stack } from '@/components/ui/stack'
import { Callout, InlineError } from '@/components/ui/status-message'
import { Eyebrow, Text } from '@/components/ui/text'
import { LINK_KINDS, LINK_KIND_HINTS, LINK_KIND_LABELS, findIdeas } from '@/lib/hypothesis-graph'
import { nodeLabel, useEditor } from '@/components/hypotheses/editor'
import type { LinkType } from '@/types/api'

/** Stands where an idea would be added to a closed experiment. */
function ClosedNote() {
  return <Text as="span" variant="hint">Closed. New ideas go into a new experiment.</Text>
}

/** The one way to start writing an idea, as a new approach to the objective; while writing, it turns the idea back to the objective. */
export function AddIdeaButton() {
  const editor = useEditor()
  if (!editor) return null
  if (!editor.canWrite) return editor.closed ? <ClosedNote /> : null
  return <Button size="sm" variant="primary" onClick={() => editor.startAdding(editor.graph.root)}>Add idea</Button>
}

export function addHereLabel(objective: boolean): string {
  return objective ? 'Add a new approach to the objective' : 'Add an idea refining this one'
}

/** The small "+" that opens the composer for one node, shown while the pointer or focus is on its row. */
export function AddHereButton({ id, className, style }: { id: string; className?: string; style?: CSSProperties }) {
  const editor = useEditor()
  if (!editor?.canWrite || editor.picking) return null
  const label = addHereLabel(id === editor.graph.root)
  return (
    <Button
      variant="primary"
      size="icon"
      className={clsx('nodrag nopan opacity-0 focus-visible:opacity-100 group-focus-within:opacity-100 group-hover:opacity-100', className)}
      style={style}
      aria-label={label}
      title={label}
      onClick={e => {
        e.stopPropagation()
        editor.startAdding(id)
      }}
    >
      <Plus size={12} />
    </Button>
  )
}

/** Starts linking an idea to another. */
export function LinkButton({ id }: { id: string }) {
  const editor = useEditor()
  if (!editor?.canWrite || editor.picking || editor.composing || id === editor.graph.root) return null
  return <Button size="sm" onClick={() => editor.startLinking(id)}>Link to…</Button>
}

/** Finds a node of the graph by its text, without a round trip, and hands the chosen one back. */
export function IdeaSearch({ placeholder = 'Find an idea by its text' }: { placeholder?: string }) {
  const editor = useEditor()
  const [q, setQ] = useState('')
  if (!editor) return null
  const { graph } = editor
  const matches = [
    ...(q === '' || 'objective'.includes(q.toLowerCase()) ? [{ id: graph.root, text: `Objective: ${editor.rootLabel}`, status: undefined }] : []),
    ...findIdeas(graph, q),
  ].filter(n => editor.canPick(n.id)).slice(0, 8)
  return (
    <Stack gap="xs">
      <Input fieldSize="sm" type="search" value={q} onChange={e => setQ(e.target.value)} placeholder={placeholder} aria-label={placeholder} autoFocus />
      {matches.length === 0 ? (
        <Text variant="hint">No idea to link here matches.</Text>
      ) : (
        <Stack as="ul" gap="2xs" className="max-h-48 list-none overflow-y-auto">
          {matches.map(n => (
            <li key={n.id}>
              <RowButton className="text-xs" onClick={() => editor.pick(n.id)} title={n.text}>
                {n.status && <Badge status={n.status}>{n.status}</Badge>}
                <span className="min-w-0 flex-1 truncate">{n.text}</span>
              </RowButton>
            </li>
          ))}
        </Stack>
      )}
    </Stack>
  )
}

function KindChips({ value, onChange, kinds = LINK_KINDS }: {
  value: LinkType | null
  onChange: (kind: LinkType) => void
  kinds?: LinkType[]
}) {
  return (
    <Inline gap="xs" role="group" aria-label="Kind of link">
      {kinds.map(k => <Chip key={k} active={value === k} title={LINK_KIND_HINTS[k]} onClick={() => onChange(k)}>{LINK_KIND_LABELS[k]}</Chip>)}
    </Inline>
  )
}

/** Linking a selected idea to another: pick the kind, then click the other in the view or find it. */
export function LinkBar() {
  const editor = useEditor()
  if (!editor?.linking) return null
  const { linking } = editor
  return (
    <Stack gap="sm" className="border-b bg-surface-raised px-3 py-2.5 text-xs">
      <Inline>
        <Eyebrow>Link</Eyebrow>
        <span className="min-w-0 max-w-md truncate" title={nodeLabel(editor, linking.from)}>{nodeLabel(editor, linking.from)}</span>
        <KindChips value={linking.kind} onChange={editor.setLinkKind} />
        <span className="text-muted">then click the other idea, or find it below.</span>
        <Button variant="link" size="sm" className="ml-auto" onClick={editor.cancel}>Cancel</Button>
      </Inline>
      <div className="max-w-xl"><IdeaSearch /></div>
      {editor.linkBusy && <span className="text-muted">Linking…</span>}
      {editor.linkError && <InlineError>{editor.linkError}</InlineError>}
    </Stack>
  )
}

/** After an idea is added: the ones already there it reads close to, or the refuted ones it refines. */
export function AddedNote() {
  const editor = useEditor()
  if (!editor || editor.addedSimilar.length === 0) return null
  const similar = editor.addedSimilar
  return (
    <Callout tone="warning">
      <Stack gap="xs" className="text-xs" role="note" aria-label="Similar ideas">
        <Inline nowrap>
          <span className="flex-1">Similar ideas already on the graph:</span>
          <Button variant="ghost" size="icon" aria-label="Dismiss" onClick={editor.dismissSimilar}><X size={12} /></Button>
        </Inline>
        <Stack as="ul" gap="2xs" className="list-none">
          {similar.map(h => (
            <Inline as="li" gap="xs" nowrap key={h.id}>
              <Badge status={h.status}>{h.status}</Badge>
              <Button variant="link" size="sm" className="min-w-0 flex-1 justify-start truncate" title={h.text} onClick={() => editor.select(h.id)}>
                {h.text}
              </Button>
            </Inline>
          ))}
        </Stack>
        {similar.some(h => h.reason === 'refuted_parent') && <span className="text-subtle">Yours refines a refuted idea.</span>}
      </Stack>
    </Callout>
  )
}

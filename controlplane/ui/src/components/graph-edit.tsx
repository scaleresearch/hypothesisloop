'use client'

import { createContext, useCallback, useContext, useEffect, useMemo, useState } from 'react'
import type { CSSProperties } from 'react'
import type { KeyedMutator } from 'swr'
import { Plus, X } from 'lucide-react'
import { clsx } from 'clsx'
import { addHumanHypothesisLink, registerHumanHypothesis } from '@/lib/api'
import type { RegisteredHypothesis, SimilarHypothesis } from '@/lib/api'
import type { HypothesisGraph, HypothesisLinkInput, HypothesisLinkType } from '@/types'
import { Badge } from '@/components/ui/badge'
import { Button, Chip, RowButton } from '@/components/ui/button'
import { Input, Textarea } from '@/components/ui/input'
import { Inline, Stack } from '@/components/ui/stack'
import { Callout, InlineError } from '@/components/ui/status-message'
import { Eyebrow, Text } from '@/components/ui/text'
import { LINK_KINDS, LINK_KIND_HINTS, LINK_KIND_LABELS, allowedLinkKinds, findIdeas, linkPlanner, withEdge, withHypothesis } from '@/lib/hypothesis-graph'

export interface GraphEditor {
  graph: HypothesisGraph
  rootLabel: string
  /** A person of the experiment's org, while it is not closed. */
  canWrite: boolean
  /** The experiment is closed: the platform refuses new ideas, and the views say so where they would be added. */
  closed: boolean
  /** An idea is being written. */
  composing: boolean
  /** Opens the composer with its first link refining `target`, or a new approach when it is the objective. */
  startAdding: (target: string) => void
  /** The links of the idea being written, starting with builds_on to the node it was opened for. */
  links: HypothesisLinkInput[]
  setLinkType: (to: string, type: HypothesisLinkType) => void
  removeLink: (to: string) => void
  /** A target for another link of the idea being written is being picked. */
  findingTarget: boolean
  setFindingTarget: (finding: boolean) => void
  linking: { from: string; kind: HypothesisLinkType } | null
  startLinking: (from: string) => void
  setLinkKind: (kind: HypothesisLinkType) => void
  linkBusy: boolean
  linkError: string
  /** Waiting for a click on, or a choice of, a target node. */
  picking: boolean
  canPick: (id: string) => boolean
  pick: (id: string) => void
  added: (h: RegisteredHypothesis) => Promise<void>
  /** Stops adding or linking and selects the node. */
  show: (id: string) => void
  cancel: () => void
}

export function useGraphEditor({ graph, rootLabel, canWrite, closed = false, mutate, onSelect }: {
  graph: HypothesisGraph
  rootLabel: string
  canWrite: boolean
  closed?: boolean
  mutate: KeyedMutator<HypothesisGraph>
  onSelect: (id: string) => void
}): GraphEditor {
  const [composing, setComposing] = useState(false)
  const [links, setLinks] = useState<HypothesisLinkInput[]>([])
  const [findingTarget, setFindingTarget] = useState(false)
  const [linking, setLinking] = useState<{ from: string; kind: HypothesisLinkType } | null>(null)
  const [linkBusy, setLinkBusy] = useState(false)
  const [linkError, setLinkError] = useState('')
  const plan = useMemo(() => linkPlanner(graph), [graph])

  const cancel = useCallback(() => {
    setComposing(false)
    setLinks([])
    setFindingTarget(false)
    setLinking(null)
    setLinkError('')
  }, [])

  const picking = linking !== null || (composing && findingTarget)

  useEffect(() => {
    if (!composing && linking === null) return
    function onKey(e: KeyboardEvent) {
      if (e.key !== 'Escape') return
      if (findingTarget) setFindingTarget(false)
      else cancel()
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [composing, linking, findingTarget, cancel])

  function targetAllowed(id: string): boolean {
    return composing && findingTarget && !links.some(l => l.to === id)
  }

  function canPick(id: string): boolean {
    if (linking) return !('refused' in plan(linking.from, id, linking.kind))
    return targetAllowed(id)
  }

  async function link(to: string) {
    if (!linking) return
    const p = plan(linking.from, to, linking.kind)
    if ('refused' in p) {
      setLinkError(p.refused)
      return
    }
    setLinkBusy(true)
    setLinkError('')
    try {
      const stored = await addHumanHypothesisLink(p.from, linking.kind, p.to)
      const made = stored.find(l => l.type === linking.kind && l.hypothesis_id === p.to)
      if (made) {
        const edge = { from: p.from, to: p.to, type: made.type, agent_id: made.agent_id, actor: made.actor, created_at: made.created_at }
        await mutate(g => g && withEdge(g, edge), { revalidate: true })
      } else {
        await mutate()
      }
      const from = linking.from
      cancel()
      onSelect(from)
    } catch (e) {
      setLinkError(e instanceof Error ? e.message : String(e))
    } finally {
      setLinkBusy(false)
    }
  }

  function pick(id: string) {
    if (linking) {
      void link(id)
      return
    }
    if (!targetAllowed(id)) return
    setLinks([...links, { type: 'builds_on', to: id }])
    setFindingTarget(false)
  }

  const startAdding = useCallback((target: string) => {
    cancel()
    setComposing(true)
    setLinks([{ type: 'builds_on', to: target }])
  }, [cancel])

  async function added(h: RegisteredHypothesis) {
    const links = h.links ?? []
    const node = {
      ...h,
      builds_on: links.filter(l => l.type === 'builds_on').map(l => l.hypothesis_id),
      child_count: 0, finding_count: 0, comment_count: 0,
    }
    const edges = links.map(l => ({ from: h.id, to: l.hypothesis_id, type: l.type, agent_id: l.agent_id, actor: l.actor, created_at: l.created_at }))
    const drawn = mutate(g => g && withHypothesis(g, node, edges), { revalidate: true })
    cancel()
    onSelect(h.id)
    await drawn
  }

  return {
    graph,
    rootLabel,
    canWrite,
    closed,
    composing: canWrite && composing,
    startAdding,
    links,
    setLinkType: (to, type) => {
      if (allowedLinkKinds(graph, to).includes(type)) setLinks(links.map(l => (l.to === to ? { ...l, type } : l)))
    },
    removeLink: to => setLinks(links.filter(l => l.to !== to)),
    findingTarget,
    setFindingTarget,
    linking: canWrite ? linking : null,
    startLinking: from => {
      cancel()
      setLinking({ from, kind: 'related' })
    },
    setLinkKind: kind => {
      setLinkError('')
      setLinking(l => l && { ...l, kind })
    },
    linkBusy,
    linkError,
    picking: canWrite && picking,
    canPick,
    pick,
    added,
    show: id => {
      cancel()
      onSelect(id)
    },
    cancel,
  }
}

const EditorContext = createContext<GraphEditor | null>(null)

export const GraphEditorProvider = EditorContext.Provider

export function useEditor(): GraphEditor | null {
  return useContext(EditorContext)
}

function nodeLabel(editor: GraphEditor, id: string): string {
  if (id === editor.graph.root) return `Objective: ${editor.rootLabel}`
  return editor.graph.nodes.find(n => n.id === id)?.text ?? id
}

/** Stands where an idea would be added to a closed experiment. */
export function ClosedNote() {
  return <Text as="span" variant="hint">Closed. New ideas go into a new experiment.</Text>
}

/** The one way to start writing an idea, opened as a new approach to the objective. */
export function AddIdeaButton() {
  const editor = useEditor()
  if (!editor) return null
  if (!editor.canWrite) return editor.closed ? <ClosedNote /> : null
  if (editor.composing) return null
  return <Button size="sm" variant="primary" onClick={() => editor.startAdding(editor.graph.root)}>Add idea</Button>
}

export function addHereLabel(objective: boolean): string {
  return objective ? 'Add a new approach to the objective' : 'Add an idea refining this one'
}

/** The small "+" that opens the composer for one node, shown while the pointer or focus is on its row. */
export function AddHereButton({ id, className, style }: { id: string; className?: string; style?: CSSProperties }) {
  const editor = useEditor()
  if (!editor?.canWrite || editor.composing || editor.picking) return null
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
  value: HypothesisLinkType | null
  onChange: (kind: HypothesisLinkType) => void
  kinds?: HypothesisLinkType[]
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

type Choice = 'related' | 'contradicts' | 'distinct'

const CHOICES: Array<{ value: Choice; label: string }> = [
  { value: 'related', label: 'Link to it' },
  { value: 'contradicts', label: 'Mine contradicts it' },
  { value: 'distinct', label: 'Keep mine distinct' },
]

function SimilarIdeas({ similar, choices, onChoose, onShow }: {
  similar: SimilarHypothesis[]
  choices: Record<string, Choice>
  onChoose: (id: string, choice: Choice) => void
  onShow: (id: string) => void
}) {
  return (
    <Callout tone="warning">
      <Stack gap="sm" className="text-xs">
      <span className="text-muted">
        Not added yet: these read close to your idea, or it refines a refuted one. Link yours to each, or keep it distinct.
      </span>
      <Stack as="ul" gap="sm" className="list-none">
        {similar.map(h => (
          <Stack as="li" gap="xs" key={h.id}>
            <Inline nowrap>
              <Badge status={h.status}>{h.status}</Badge>
              <span className="min-w-0 flex-1 truncate" title={h.text}>{h.text}</span>
              <Button variant="link" size="sm" onClick={() => onShow(h.id)} title="It is my idea: show it and add nothing">Show it</Button>
            </Inline>
            {h.reason === 'refuted_parent' ? (
              <Inline gap="xs" nowrap>
                <span className="text-subtle">Yours refines this refuted idea.</span>
                <Chip active={choices[h.id] === 'distinct'} onClick={() => onChoose(h.id, 'distinct')}>Refine it anyway</Chip>
              </Inline>
            ) : (
              <Inline gap="xs" role="group" aria-label={`Choice for ${h.text}`}>
                {CHOICES.map(c => <Chip key={c.value} active={choices[h.id] === c.value} onClick={() => onChoose(h.id, c.value)}>{c.label}</Chip>)}
              </Inline>
            )}
          </Stack>
        ))}
      </Stack>
      </Stack>
    </Callout>
  )
}

/**
 * Writing a new idea, the same beside the graph and the outline: its text, its links (builds_on to
 * the node it was opened for to start with), and the choice the API asks for when it reads close to
 * ideas already there.
 */
export function IdeaEditor() {
  const editor = useEditor()
  const [text, setText] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const [existingID, setExistingID] = useState('')
  const [similar, setSimilar] = useState<SimilarHypothesis[]>([])
  const [choices, setChoices] = useState<Record<string, Choice>>({})
  if (!editor?.composing) return null
  const ed = editor

  async function submit(chosen: Record<string, Choice>) {
    setBusy(true)
    setError('')
    setExistingID('')
    try {
      const told = Object.entries(chosen)
      const h = await registerHumanHypothesis(ed.graph.root, text.trim(), [
        ...ed.links,
        ...told.filter(([, c]) => c !== 'distinct').map(([to, c]) => ({ type: c as HypothesisLinkType, to })),
      ], told.filter(([, c]) => c === 'distinct').map(([id]) => id))
      if ('similar' in h) {
        setSimilar(h.similar)
        setChoices(Object.fromEntries(told.filter(([id]) => h.similar.some(s => s.id === id))))
      } else if (h.already_existed) {
        setExistingID(h.id)
      } else {
        await ed.added(h)
      }
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
    } finally {
      setBusy(false)
    }
  }

  const undecided = similar.filter(h => !choices[h.id])
  const keepAll = () => {
    const all = { ...choices, ...Object.fromEntries(undecided.map(h => [h.id, 'distinct' as Choice])) }
    setChoices(all)
    void submit(all)
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
      <Text variant="meta" tone="subtle">
        It joins the graph under your name. Agents read it, and one that tests it registers its own idea refining yours.
      </Text>
      {similar.length > 0 && (
        <SimilarIdeas
          similar={similar}
          choices={choices}
          onChoose={(id, c) => setChoices({ ...choices, [id]: c })}
          onShow={ed.show}
        />
      )}
      {existingID && (
        <Text as="span" variant="note">
          An equivalent idea already exists, so nothing was added.{' '}
          <Button variant="link" size="sm" onClick={() => ed.show(existingID)}>Show it</Button>
        </Text>
      )}
      {error && <InlineError>{error}</InlineError>}
      <Inline>
        <Button
          size="sm"
          variant="primary"
          loading={busy}
          disabled={text.trim() === '' || !buildsOn || undecided.length > 0}
          onClick={() => submit(choices)}
        >
          Add idea
        </Button>
        {undecided.length > 0 && (
          <Button size="sm" disabled={busy} onClick={keepAll}>Keep mine distinct from all</Button>
        )}
        <Button variant="link" size="sm" className="ml-auto" onClick={ed.cancel}>Cancel</Button>
      </Inline>
    </Stack>
  )
}

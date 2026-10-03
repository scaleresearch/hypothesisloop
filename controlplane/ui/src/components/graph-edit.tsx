'use client'

import { createContext, useCallback, useContext, useEffect, useMemo, useState } from 'react'
import type { KeyedMutator } from 'swr'
import { X } from 'lucide-react'
import { addHumanHypothesisLink, registerHumanHypothesis } from '@/lib/api'
import type { RegisteredHypothesis, SimilarHypothesis } from '@/lib/api'
import type { HypothesisGraph, HypothesisLinkInput, HypothesisLinkType } from '@/types'
import { Badge } from '@/components/ui/badge'
import { Button, Chip, RowButton } from '@/components/ui/button'
import { Input, Textarea } from '@/components/ui/input'
import { InlineError } from '@/components/ui/status-message'
import { Eyebrow } from '@/components/ui/text'
import { LINK_KINDS, LINK_KIND_LABELS, findIdeas, linkPlanner, withEdge, withHypothesis } from '@/lib/hypothesis-graph'

export interface GraphEditor {
  graph: HypothesisGraph
  rootLabel: string
  /** A person of the experiment's org, while it is not closed. */
  canWrite: boolean
  /** The node the idea being written builds on. */
  adding: string | null
  startAdding: (parent: string) => void
  /** The extra links of the idea being written. */
  extras: HypothesisLinkInput[]
  removeExtra: (to: string) => void
  /** The kind of extra link whose target is being picked. */
  extraKind: HypothesisLinkType | null
  setExtraKind: (kind: HypothesisLinkType | null) => void
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

export function useGraphEditor({ graph, rootLabel, canWrite, mutate, onSelect }: {
  graph: HypothesisGraph
  rootLabel: string
  canWrite: boolean
  mutate: KeyedMutator<HypothesisGraph>
  onSelect: (id: string) => void
}): GraphEditor {
  const [adding, setAdding] = useState<string | null>(null)
  const [extras, setExtras] = useState<HypothesisLinkInput[]>([])
  const [extraKind, setExtraKind] = useState<HypothesisLinkType | null>(null)
  const [linking, setLinking] = useState<{ from: string; kind: HypothesisLinkType } | null>(null)
  const [linkBusy, setLinkBusy] = useState(false)
  const [linkError, setLinkError] = useState('')
  const plan = useMemo(() => linkPlanner(graph), [graph])

  const cancel = useCallback(() => {
    setAdding(null)
    setExtras([])
    setExtraKind(null)
    setLinking(null)
    setLinkError('')
  }, [])

  const picking = linking !== null || (adding !== null && extraKind !== null)

  useEffect(() => {
    if (adding === null && linking === null) return
    function onKey(e: KeyboardEvent) {
      if (e.key !== 'Escape') return
      if (extraKind !== null) setExtraKind(null)
      else cancel()
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [adding, linking, extraKind, cancel])

  function extraAllowed(id: string): boolean {
    if (extraKind === null || adding === null) return false
    if (id === graph.root) return extraKind === 'builds_on' && adding !== graph.root && !extras.some(l => l.to === id)
    return id !== adding && !extras.some(l => l.to === id)
  }

  function canPick(id: string): boolean {
    if (linking) return !('refused' in plan(linking.from, id, linking.kind))
    return extraAllowed(id)
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
    if (!extraAllowed(id)) return
    setExtras([...extras, { type: extraKind!, to: id }])
    setExtraKind(null)
  }

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
    adding: canWrite ? adding : null,
    startAdding: parent => {
      cancel()
      setAdding(parent)
    },
    extras,
    removeExtra: to => setExtras(extras.filter(l => l.to !== to)),
    extraKind,
    setExtraKind,
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
  if (id === editor.graph.root) return `the objective of ${editor.rootLabel}`
  return editor.graph.nodes.find(n => n.id === id)?.text ?? id
}

/** The two things a person does at a node: add an idea that builds on it, and link it to another. */
export function NodeActions({ id }: { id: string }) {
  const editor = useEditor()
  if (!editor?.canWrite || editor.picking || editor.adding !== null) return null
  const root = id === editor.graph.root
  return (
    <div className="flex items-center gap-1.5">
      <Button
        size="sm"
        variant="primary"
        title={root ? 'A foundational idea, building on the objective' : 'An idea that builds on this one'}
        onClick={() => editor.startAdding(id)}
      >
        Add idea here
      </Button>
      {!root && <Button size="sm" onClick={() => editor.startLinking(id)}>Link to…</Button>}
    </div>
  )
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
    <div className="grid grid-cols-[minmax(0,1fr)] gap-1">
      <Input fieldSize="sm" type="search" value={q} onChange={e => setQ(e.target.value)} placeholder={placeholder} aria-label={placeholder} autoFocus />
      {matches.length === 0 ? (
        <p className="text-xs text-subtle">No idea to link here matches.</p>
      ) : (
        <ul className="grid max-h-48 list-none grid-cols-[minmax(0,1fr)] gap-0.5 overflow-y-auto">
          {matches.map(n => (
            <li key={n.id}>
              <RowButton className="text-xs" onClick={() => editor.pick(n.id)} title={n.text}>
                {n.status && <Badge status={n.status}>{n.status}</Badge>}
                <span className="min-w-0 flex-1 truncate">{n.text}</span>
              </RowButton>
            </li>
          ))}
        </ul>
      )}
    </div>
  )
}

function KindChips({ value, onChange, kinds = LINK_KINDS }: {
  value: HypothesisLinkType | null
  onChange: (kind: HypothesisLinkType) => void
  kinds?: HypothesisLinkType[]
}) {
  return (
    <div className="flex flex-wrap gap-1.5" role="group" aria-label="Kind of link">
      {kinds.map(k => <Chip key={k} active={value === k} onClick={() => onChange(k)}>{LINK_KIND_LABELS[k]}</Chip>)}
    </div>
  )
}

/** Linking a selected idea to another: pick the kind, then click the other in the view or find it. */
export function LinkBar() {
  const editor = useEditor()
  if (!editor?.linking) return null
  const { linking } = editor
  return (
    <div className="grid grid-cols-[minmax(0,1fr)] gap-2 border-b bg-surface-raised px-3 py-2.5 text-xs">
      <div className="flex flex-wrap items-center gap-2">
        <Eyebrow>Link</Eyebrow>
        <span className="min-w-0 max-w-[420px] truncate" title={nodeLabel(editor, linking.from)}>{nodeLabel(editor, linking.from)}</span>
        <KindChips value={linking.kind} onChange={editor.setLinkKind} />
        <span className="text-muted">then click the other idea, or find it below.</span>
        <Button variant="link" size="sm" className="ml-auto" onClick={editor.cancel}>Cancel</Button>
      </div>
      <div className="max-w-[560px]"><IdeaSearch /></div>
      {editor.linkBusy && <span className="text-muted">Linking…</span>}
      {editor.linkError && <InlineError>{editor.linkError}</InlineError>}
    </div>
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
    <div className="grid grid-cols-[minmax(0,1fr)] gap-2 rounded border border-warning-edge p-2 text-xs">
      <span className="text-muted">
        Not added yet: these read close to your idea, or it builds on a refuted one. Link yours to each, or keep it distinct.
      </span>
      <ul className="grid list-none grid-cols-[minmax(0,1fr)] gap-2">
        {similar.map(h => (
          <li key={h.id} className="grid grid-cols-[minmax(0,1fr)] gap-1">
            <div className="flex items-center gap-2">
              <Badge status={h.status}>{h.status}</Badge>
              <span className="min-w-0 flex-1 truncate" title={h.text}>{h.text}</span>
              <Button variant="link" size="sm" onClick={() => onShow(h.id)} title="It is my idea: show it and add nothing">Show it</Button>
            </div>
            {h.reason === 'refuted_parent' ? (
              <div className="flex items-center gap-1.5">
                <span className="text-subtle">Yours builds on this refuted idea.</span>
                <Chip active={choices[h.id] === 'distinct'} onClick={() => onChoose(h.id, 'distinct')}>Build on it anyway</Chip>
              </div>
            ) : (
              <div className="flex flex-wrap gap-1.5" role="group" aria-label={`Choice for ${h.text}`}>
                {CHOICES.map(c => <Chip key={c.value} active={choices[h.id] === c.value} onClick={() => onChoose(h.id, c.value)}>{c.label}</Chip>)}
              </div>
            )}
          </li>
        ))}
      </ul>
    </div>
  )
}

/**
 * Writing a new idea in place, under the node it builds on: its text, extra links picked from the
 * view, and the choice the API asks for when it reads close to ideas already there.
 */
export function IdeaEditor() {
  const editor = useEditor()
  const [text, setText] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const [existingID, setExistingID] = useState('')
  const [similar, setSimilar] = useState<SimilarHypothesis[]>([])
  const [choices, setChoices] = useState<Record<string, Choice>>({})
  if (!editor || editor.adding === null) return null
  const ed = editor
  const parent = ed.adding!

  async function submit(chosen: Record<string, Choice>) {
    setBusy(true)
    setError('')
    setExistingID('')
    try {
      const told = Object.entries(chosen)
      const h = await registerHumanHypothesis(ed.graph.root, text.trim(), [
        { type: 'builds_on', to: parent },
        ...ed.extras,
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

  return (
    <div className="grid w-[340px] grid-cols-[minmax(0,1fr)] gap-2 rounded-lg border border-primary bg-surface-raised p-3 shadow-lg" role="group" aria-label="New idea">
      <div className="text-xs">
        <Eyebrow>Builds on</Eyebrow>{' '}
        <span className="line-clamp-2" title={nodeLabel(ed, parent)}>{nodeLabel(ed, parent)}</span>
      </div>
      <Textarea
        className="min-h-[70px] w-full text-sm"
        value={text}
        onChange={e => setText(e.target.value)}
        placeholder="e.g. the retry loop is masking a bad learning-rate schedule; try a linear warmup instead"
        aria-label="Your idea"
        autoFocus
      />
      <p className="text-2xs text-subtle">
        It joins the graph under your name. Agents read it, and one that tests it registers its own idea building on yours.
      </p>
      {ed.extras.length > 0 && (
        <ul className="grid list-none grid-cols-[minmax(0,1fr)] gap-0.5 text-xs">
          {ed.extras.map(l => (
            <li key={l.to} className="flex items-center gap-1.5">
              <span className="whitespace-nowrap text-muted">also {LINK_KIND_LABELS[l.type]}</span>
              <span className="min-w-0 flex-1 truncate" title={nodeLabel(ed, l.to)}>{nodeLabel(ed, l.to)}</span>
              <Button variant="ghost" size="icon" aria-label={`Remove link to ${nodeLabel(ed, l.to)}`} onClick={() => ed.removeExtra(l.to)}><X size={12} /></Button>
            </li>
          ))}
        </ul>
      )}
      {ed.extraKind === null ? (
        <div>
          <Button variant="link" size="sm" onClick={() => ed.setExtraKind('related')}>+ Link it to another idea</Button>
        </div>
      ) : (
        <div className="grid grid-cols-[minmax(0,1fr)] gap-1.5 text-xs">
          <div className="flex items-center gap-2">
            <span className="text-muted">It</span>
            <KindChips value={ed.extraKind} onChange={ed.setExtraKind} />
            <Button variant="link" size="sm" className="ml-auto" onClick={() => ed.setExtraKind(null)}>Done</Button>
          </div>
          <span className="text-subtle">Click the idea in the view, or find it:</span>
          <IdeaSearch />
        </div>
      )}
      {similar.length > 0 && (
        <SimilarIdeas
          similar={similar}
          choices={choices}
          onChoose={(id, c) => setChoices({ ...choices, [id]: c })}
          onShow={ed.show}
        />
      )}
      {existingID && (
        <span className="text-xs text-muted">
          An equivalent idea already exists, so nothing was added.{' '}
          <Button variant="link" size="sm" onClick={() => ed.show(existingID)}>Show it</Button>
        </span>
      )}
      {error && <InlineError>{error}</InlineError>}
      <div className="flex flex-wrap items-center gap-2">
        <Button
          size="sm"
          variant="primary"
          loading={busy}
          disabled={text.trim() === '' || undecided.length > 0}
          onClick={() => submit(choices)}
        >
          Add idea
        </Button>
        {undecided.length > 0 && (
          <Button size="sm" disabled={busy} onClick={keepAll}>Keep mine distinct from all</Button>
        )}
        <Button variant="link" size="sm" className="ml-auto" onClick={ed.cancel}>Cancel</Button>
      </div>
    </div>
  )
}

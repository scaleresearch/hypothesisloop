import { createContext, useCallback, useContext, useEffect, useMemo, useState } from 'react'
import type { KeyedMutator } from 'swr'
import { addHumanHypothesisLink } from '@/lib/api/hypotheses'
import type { HypothesisGraph, HypothesisLinkInput, HypothesisLinkType, RegisteredHypothesis, SimilarHypothesis } from '@/types/hypotheses'
import { allowedLinkKinds, linkPlanner, withEdge, withHypothesis } from '@/lib/hypothesis-graph'

interface GraphEditor {
  graph: HypothesisGraph
  /** The ids some link of the graph points to. */
  linked: Set<string>
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
  /** Selects the node and keeps the idea being written. */
  select: (id: string) => void
  /** What the idea just added reads close to, as the platform answered when it registered it. */
  addedSimilar: SimilarHypothesis[]
  dismissSimilar: () => void
  /** Reads the graph again after an idea changed. */
  refresh: () => Promise<unknown>
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
  const [addedSimilar, setAddedSimilar] = useState<SimilarHypothesis[]>([])
  const [links, setLinks] = useState<HypothesisLinkInput[]>([])
  const [findingTarget, setFindingTarget] = useState(false)
  const [linking, setLinking] = useState<{ from: string; kind: HypothesisLinkType } | null>(null)
  const [linkBusy, setLinkBusy] = useState(false)
  const [linkError, setLinkError] = useState('')
  const plan = useMemo(() => linkPlanner(graph), [graph])
  const linked = useMemo(() => new Set(graph.edges.map(e => e.to)), [graph])

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
    // On the document, ahead of the window, so the graph stays in the whole window while Escape cancels an edit.
    function onKey(e: KeyboardEvent) {
      if (e.key !== 'Escape') return
      e.preventDefault()
      if (findingTarget) setFindingTarget(false)
      else cancel()
    }
    document.addEventListener('keydown', onKey)
    return () => document.removeEventListener('keydown', onKey)
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

  // While an idea is being written its text stays, and its first link becomes refining the target.
  const startAdding = useCallback((target: string) => {
    setLinking(null)
    setLinkError('')
    setFindingTarget(false)
    setAddedSimilar([])
    setComposing(true)
    const first: HypothesisLinkInput = { type: 'builds_on', to: target }
    setLinks(ls => (composing ? [first, ...ls.slice(1).filter(l => l.to !== target)] : [first]))
  }, [composing])

  async function added({ similar, ...h }: RegisteredHypothesis) {
    const links = h.links ?? []
    const node = {
      ...h,
      builds_on: links.filter(l => l.type === 'builds_on').map(l => l.hypothesis_id),
      child_count: 0, finding_count: 0, comment_count: 0,
    }
    const edges = links.map(l => ({ from: h.id, to: l.hypothesis_id, type: l.type, agent_id: l.agent_id, actor: l.actor, created_at: l.created_at }))
    const drawn = mutate(g => g && withHypothesis(g, node, edges), { revalidate: true })
    cancel()
    setAddedSimilar(similar ?? [])
    onSelect(h.id)
    await drawn
  }

  return useMemo<GraphEditor>(() => ({
    graph,
    linked,
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
    select: onSelect,
    addedSimilar,
    dismissSimilar: () => setAddedSimilar([]),
    refresh: () => mutate(),
    cancel,
    // eslint-disable-next-line react-hooks/exhaustive-deps -- its functions read only these
  }), [graph, linked, rootLabel, canWrite, closed, composing, links, findingTarget, linking, linkBusy, linkError, addedSimilar, plan, mutate, onSelect, cancel, startAdding])
}

const EditorContext = createContext<GraphEditor | null>(null)

export const GraphEditorProvider = EditorContext.Provider

export function useEditor(): GraphEditor | null {
  return useContext(EditorContext)
}

export function nodeLabel(editor: GraphEditor, id: string): string {
  if (id === editor.graph.root) return `Objective: ${editor.rootLabel}`
  return editor.graph.nodes.find(n => n.id === id)?.text ?? id
}

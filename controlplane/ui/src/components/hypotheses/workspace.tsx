'use client'

import { useEffect, useMemo, useRef, useState } from 'react'
import type { ReactNode } from 'react'
import { useRouter } from 'next/navigation'
import useSWR from 'swr'
import type { KeyedMutator } from 'swr'
import { clsx } from 'clsx'
import { fetchHypothesisGraph, type HypothesisSearchHit } from '@/lib/api/hypotheses'
import { fetchPlatformExperiment } from '@/lib/api/platform-experiments'
import { Page } from '@/components/ui/page'
import { Card } from '@/components/ui/card'
import { Expandable } from '@/components/ui/expandable'
import { Button, Chip } from '@/components/ui/button'
import { FormField } from '@/components/ui/form-field'
import { Section } from '@/components/ui/section'
import { Inline, Stack } from '@/components/ui/stack'
import { Text } from '@/components/ui/text'
import { Loading, ErrorMessage } from '@/components/ui/status-message'
import { SearchableSelect } from '@/components/ui/searchable-select'
import { PlatformExperimentSelect } from '@/components/platform-experiment-select'
import { AgentNames, useAgentName } from '@/components/agent-label'
import { GraphEditorProvider, useGraphEditor } from '@/components/hypotheses/editor'
import { AddIdeaButton, AddedNote, LinkBar } from '@/components/hypotheses/editor-controls'
import { IdeaEditor } from '@/components/hypotheses/idea-editor'
import { HypothesisGraphCanvas } from '@/components/hypotheses/graph-canvas'
import { HypothesisOutline } from '@/components/hypotheses/outline'
import { HypothesisPanel, ObjectiveBar } from '@/components/hypotheses/panel'
import { SearchBox, SearchHits, useHypothesisSearch } from '@/components/hypotheses/search'
import { useQueryParam } from '@/lib/query-state'
import { withQuery } from '@/lib/routes'
import { ExperimentActionsProvider } from '@/lib/experiment-actions'
import { HUMAN_OWNER, extendLayout, graphHref, layoutGraph, matchesFilters, shareUnchanged } from '@/lib/hypothesis-graph'
import type { GraphFilters, Position } from '@/lib/hypothesis-graph'
import { keys } from '@/lib/keys'
import type { HypothesisGraph, PlatformExperiment } from '@/types/api'

/** The view a hypotheses page opens in, which the URL leaves out. */
export const HYPOTHESES_VIEW = 'outline'

const STATUS_OPTIONS = ['open', 'confirmed', 'refuted', 'inconclusive'].map(s => ({ value: s, label: s }))

/** Narrows the graph to one agent's hypotheses, or to the ones people added. */
function OwnerFilter({ graph, value, onChange }: { graph: HypothesisGraph | undefined; value: string; onChange: (owner: string) => void }) {
  const agentName = useAgentName()
  const options = useMemo(() => {
    const nodes = graph?.nodes ?? []
    const agents = Array.from(new Set(nodes.map(n => n.agent_id).filter(Boolean)))
    const listed = agents.map(a => ({ value: a, label: agentName(a) })).sort((a, b) => a.label.localeCompare(b.label))
    if (nodes.some(n => !n.agent_id && n.actor.kind === 'user')) listed.push({ value: HUMAN_OWNER, label: 'People' })
    return listed
  }, [graph, agentName])
  return <SearchableSelect value={value} onChange={onChange} allLabel="All agents" options={options} />
}

interface Layout {
  /** Counts the times it was laid out whole. */
  epoch: number
  positions: Map<string, Position>
  /** Some node was placed beside the others since the graph was last laid out whole. */
  extended: boolean
}

// The graph is laid out whole when it opens and when asked to tidy; a node that joins later is
// placed beside the others, so nothing already drawn moves under the reader.
function useStableLayout(graph: HypothesisGraph) {
  const last = useRef<Layout | null>(null)
  const [epoch, setEpoch] = useState(0)
  const layout = useMemo<Layout>(() => {
    const prev = last.current
    if (!prev || prev.epoch !== epoch) return { epoch, positions: layoutGraph(graph), extended: false }
    const positions = extendLayout(graph, prev.positions)
    return { epoch, positions, extended: prev.extended || graph.nodes.some(n => positions.has(n.id) && !prev.positions.has(n.id)) }
  }, [graph, epoch])
  useEffect(() => {
    last.current = layout
  }, [layout])
  return { ...layout, tidy: () => setEpoch(e => e + 1) }
}

/**
 * One experiment's hypotheses as an outline or a graph, with its search, filters, selection and
 * editing. On the experiment's own page it heads itself and switches experiment and view; embedded
 * in the project's hypotheses page, that page does both.
 */
export function ExperimentHypotheses({ peID, embedded = false }: { peID: string; embedded?: boolean }) {
  const router = useRouter()
  const [view, setView] = useQueryParam('view', HYPOTHESES_VIEW)
  const [statusFilter, setStatusFilter] = useQueryParam('status')
  const [ownerFilter, setOwnerFilter] = useQueryParam('agent')
  const [q, setQ] = useQueryParam('q')

  const { data: pe, error: selectedError } = useSWR(keys.experiment(peID), () => fetchPlatformExperiment(peID))

  const shown = useRef<HypothesisGraph>()
  const { data: graph, error, mutate } = useSWR(
    keys.hypothesisGraph(peID),
    async () => shareUnchanged(shown.current, await fetchHypothesisGraph(peID)),
  )
  shown.current = graph
  const search = useHypothesisSearch(peID, q)
  const filters = useMemo(() => ({ status: statusFilter, owner: ownerFilter }), [statusFilter, ownerFilter])

  const filtering = statusFilter !== '' || ownerFilter !== ''
  const matching = graph ? graph.nodes.filter(n => matchesFilters(n, filters)).length : 0

  function selectExperiment(next: string) {
    router.push(withQuery(graphHref(next), { view: view === HYPOTHESES_VIEW ? undefined : view }))
  }

  const closed = pe?.status === 'closed'

  // The toolbar heads the workspace once the graph is read, with the action that adds an idea to it.
  const toolbar = (action?: ReactNode) => (
    <Stack>
    {!embedded && <Inline gap="md">
      <FormField inline label="Experiment">
        <PlatformExperimentSelect
          value={peID}
          onChange={selectExperiment}
          hideAllOption
          placeholder="Select an experiment"
          className="min-w-64"
        />
      </FormField>
      <Inline gap="md" nowrap className="ml-auto">
        {action}
        <Inline gap="xs" nowrap role="group" aria-label="View">
          <Chip active={view === 'outline'} onClick={() => setView('outline')}>Outline</Chip>
          <Chip active={view === 'graph'} onClick={() => setView('graph')}>Graph</Chip>
        </Inline>
      </Inline>
    </Inline>}

    <Inline gap="md">
      {embedded && action}
      <SearchBox key={peID} initial={q} placeholder="Search hypotheses by text" onSearch={setQ} />
      <FormField inline label="Status" className="ml-2">
        <SearchableSelect
          value={statusFilter}
          onChange={setStatusFilter}
          allLabel="All"
          options={STATUS_OPTIONS}
        />
      </FormField>
      <FormField inline label="Agent" className="ml-2">
        <OwnerFilter graph={graph} value={ownerFilter} onChange={setOwnerFilter} />
      </FormField>
      {filtering && graph && (
        <Text as="span" variant="hint">{matching} of {graph.nodes.length} match</Text>
      )}
    </Inline>
    </Stack>
  )

  const content = (
    <>
      {!(graph && pe) && toolbar()}

      {selectedError && <ErrorMessage>Could not load experiment {peID}: {selectedError.message}</ErrorMessage>}
      {error && <ErrorMessage>Could not load the hypothesis graph: {error.message}</ErrorMessage>}
      {!(graph && pe) && !error && !selectedError && <Loading />}

      {graph && pe && (
        <Workspace
          key={graph.root}
          graph={graph}
          pe={pe}
          mutate={mutate}
          canWrite={pe.actions.includes('add_idea')}
          closed={closed}
          view={view}
          q={q}
          setQ={setQ}
          search={search}
          filters={filters}
          toolbar={toolbar}
          objective={!embedded}
        />
      )}
    </>
  )
  const page = (
    <ExperimentActionsProvider value={pe?.actions ?? []}>
      {embedded ? content : (
        <Page
          fill
          title="Hypotheses"
        >
          {content}
        </Page>
      )}
    </ExperimentActionsProvider>
  )
  return <AgentNames platformExperimentID={peID}>{page}</AgentNames>
}

function Workspace({ graph, pe, mutate, canWrite, closed, view, q, setQ, search, filters, toolbar, objective }: {
  graph: HypothesisGraph
  pe: PlatformExperiment
  mutate: KeyedMutator<HypothesisGraph>
  canWrite: boolean
  closed: boolean
  view: string
  q: string
  setQ: (q: string) => void
  search: { data?: HypothesisSearchHit[]; isLoading: boolean; error?: Error }
  filters: GraphFilters
  toolbar: (action: ReactNode) => ReactNode
  /** The objective heads the graph, unless the page around it already says what the experiment is. */
  objective: boolean
}) {
  const [focus, setFocus] = useQueryParam('focus', '', { replace: true })
  const layout = useStableLayout(graph)
  const editor = useGraphEditor({ graph, rootLabel: pe.name, canWrite, closed, mutate, onSelect: setFocus })
  const nodeByID = useMemo(() => new Map(graph.nodes.map(n => [n.id, n])), [graph])
  const hits = useMemo(() => new Set((search.data ?? []).map(h => h.id)), [search.data])

  const selected = focus || graph.root
  const showSide = q !== '' || selected !== graph.root || editor.composing

  return (
    <GraphEditorProvider value={editor}>
      {toolbar(<AddIdeaButton />)}
      {objective && <ObjectiveBar graph={graph} pe={pe} />}
      <Expandable label="Hypothesis graph" className="contents">
      <div className={clsx('grid min-h-canvas flex-fill grid-rows-1 gap-4', showSide ? 'grid-cols-with-aside' : 'grid-cols-1')}>
        <Card className="flex flex-col">
          <LinkBar />
          {view !== 'graph' ? (
            <>
              {graph.nodes.length === 0 && (
                <Text variant="note" className="px-3 pt-2.5">No hypotheses yet.</Text>
              )}
              <HypothesisOutline
                graph={graph}
                rootLabel={pe.name}
                selected={selected}
                hits={hits}
                filters={filters}
                onSelect={setFocus}
              />
            </>
          ) : (
            <HypothesisGraphCanvas
              graph={graph}
              positions={layout.positions}
              rootLabel={pe.name}
              focus={focus}
              selected={selected}
              hits={hits}
              filters={filters}
              minimap={!showSide}
              onSelect={setFocus}
              onTidy={layout.extended ? layout.tidy : undefined}
            />
          )}
        </Card>
        {showSide && <Stack as="aside" gap="xl" className="overflow-y-auto">
          <IdeaEditor />
          <AddedNote />
          {q && (
            <Section title="Search results" actions={<Button variant="link" size="sm" onClick={() => setQ('')}>Clear</Button>}>
              <SearchHits hits={search.data} onPick={h => setFocus(h.id)} isLoading={search.isLoading} error={search.error} />
            </Section>
          )}
          {selected !== graph.root && (
            <HypothesisPanel
              graph={graph}
              nodeByID={nodeByID}
              selected={selected}
              onSelect={setFocus}
            />
          )}
        </Stack>}
      </div>
      </Expandable>
    </GraphEditorProvider>
  )
}

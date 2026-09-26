'use client'

import { Suspense, useEffect, useMemo, useState } from 'react'
import { useSearchParams } from 'next/navigation'
import useSWR from 'swr'
import { clsx } from 'clsx'
import { fetchFindings, fetchHypothesisGraph, fetchPlatformExperiment, fetchPlatformExperimentsPage } from '@/lib/api'
import type { PlatformExperiment } from '@/types'
import { PageHeader } from '@/components/ui/page-header'
import { Card, CardHeader, CardContent } from '@/components/ui/card'
import { Button, Chip } from '@/components/ui/button'
import { Dialog } from '@/components/ui/dialog'
import { FormField } from '@/components/ui/form-field'
import { Loading, ErrorMessage, EmptyState } from '@/components/ui/status-message'
import { SearchableSelect } from '@/components/ui/searchable-select'
import { PlatformExperimentSelect } from '@/components/platform-experiment-select'
import { AddIdeaForm } from '@/components/human-idea'
import { HypothesisGraphCanvas } from '@/components/hypothesis-graph'
import { HypothesisOutline } from '@/components/hypothesis-outline'
import { HypothesisPanel, ObjectiveBar } from '@/components/hypothesis-panel'
import { SearchBox, SearchHits, useHypothesisSearch } from '@/components/hypothesis-search'
import { useProjectRouter, useQueryParam } from '@/lib/query-state'
import { useTenant } from '@/lib/tenant'
import { useExample } from '@/lib/example'
import { HUMAN_OWNER, graphHref, layoutGraph, matchesFilters } from '@/lib/hypothesis-graph'

const STATUS_OPTIONS = ['open', 'confirmed', 'refuted', 'inconclusive'].map(s => ({ value: s, label: s }))

export default function HypothesesPage() {
  return (
    <Suspense fallback={<Loading />}>
      <HypothesesPageContent />
    </Suspense>
  )
}

function HypothesesPageContent() {
  const { project } = useTenant()
  const router = useProjectRouter()
  const searchParams = useSearchParams()
  const example = useExample()
  // An example's graph is that experiment's, whatever the URL names.
  const peID = example ? example.id : searchParams.get('pe') ?? ''
  const [focus, setFocus] = useQueryParam('focus')
  const [view, setView] = useQueryParam('view', 'graph')
  const [statusFilter, setStatusFilter] = useQueryParam('status')
  const [ownerFilter, setOwnerFilter] = useQueryParam('agent')
  const [q, setQ] = useQueryParam('q')
  const [addUnder, setAddUnder] = useState<string | null>(null)

  const { data: platformExperiments, error: peError } = useSWR<PlatformExperiment[]>(
    peID ? null : ['platform-experiment-latest', project.id],
    () => fetchPlatformExperimentsPage({ project_id: project.id, sort: '-starts_at', limit: 1 }).then(p => p.items),
  )
  const { data: pe, error: selectedError } = useSWR(
    peID ? ['pe', peID] : null,
    () => fetchPlatformExperiment(peID),
  )

  // A graph belongs to one experiment, so the URL always names one: the latest started by default.
  useEffect(() => {
    if (!peID && platformExperiments && platformExperiments.length > 0) router.replace(graphHref(platformExperiments[0].id))
  }, [peID, platformExperiments, router])

  const { data: graph, error, mutate } = useSWR(
    peID ? ['hypothesis-graph', peID] : null,
    () => fetchHypothesisGraph(peID),
    { refreshInterval: 15_000 },
  )
  const search = useHypothesisSearch(peID, q)
  const { data: findings } = useSWR(
    peID ? ['pe-findings', peID] : null,
    () => fetchFindings(peID),
    { refreshInterval: 15_000 },
  )
  const evidence = useMemo(() => new Map((findings ?? []).map(h => [h.id, h])), [findings])

  const positions = useMemo(() => graph && layoutGraph(graph), [graph])
  const nodeByID = useMemo(() => new Map((graph?.nodes ?? []).map(n => [n.id, n])), [graph])
  const hits = useMemo(() => new Set((search.data ?? []).map(h => h.id)), [search.data])
  const filters = useMemo(() => ({ status: statusFilter, owner: ownerFilter }), [statusFilter, ownerFilter])
  const ownerOptions = useMemo(() => {
    const nodes = graph?.nodes ?? []
    const agents = Array.from(new Set(nodes.filter(n => n.source !== 'human').map(n => n.agent_id))).sort()
    const options = agents.map(a => ({ value: a, label: a }))
    if (nodes.some(n => n.source === 'human')) options.push({ value: HUMAN_OWNER, label: 'Humans' })
    return options
  }, [graph])

  const selected = focus || graph?.root || ''
  const filtering = statusFilter !== '' || ownerFilter !== ''
  const showSide = graph !== undefined && (q !== '' || selected !== graph.root)
  const matching = graph ? graph.nodes.filter(n => matchesFilters(n, filters)).length : 0

  function selectExperiment(next: string) {
    const href = graphHref(next)
    router.push(view === 'graph' ? href : `${href}&view=${view}`)
  }

  function onAdded(id: string) {
    setAddUnder(null)
    mutate()
    setFocus(id)
  }

  return (
    <div className="flex min-h-[calc(100vh-56px)] flex-col">
      <PageHeader
        title="Hypotheses"
        description="Each platform experiment grows a graph of research claims. Every hypothesis builds on the experiment's objective or on earlier hypotheses, and may contradict or relate to others. Registering text equivalent to an existing hypothesis returns the existing one; text close to others waits until the submitter has told it apart from them."
      />

      <div className="mb-2.5 flex flex-wrap items-center gap-2.5">
        {!example && (
          <FormField inline label="Platform Experiment">
            <PlatformExperimentSelect
              value={peID}
              onChange={selectExperiment}
              hideAllOption
              placeholder="Select an experiment"
              className="min-w-[260px]"
            />
          </FormField>
        )}
        <div className="ml-auto flex gap-1.5" role="group" aria-label="View">
          <Chip active={view === 'graph'} onClick={() => setView('graph')}>Graph</Chip>
          <Chip active={view === 'outline'} onClick={() => setView('outline')}>Outline</Chip>
        </div>
      </div>

      <div className="mb-3 flex flex-wrap items-center gap-2.5">
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
          <SearchableSelect
            value={ownerFilter}
            onChange={setOwnerFilter}
            allLabel="All agents"
            options={ownerOptions}
          />
        </FormField>
        {filtering && graph && (
          <span className="text-xs text-subtle">{matching} of {graph.nodes.length} match</span>
        )}
      </div>

      {peError && <ErrorMessage>Cannot reach registry service.</ErrorMessage>}
      {platformExperiments && platformExperiments.length === 0 && (
        <EmptyState>No platform experiments yet. A hypothesis graph grows inside one.</EmptyState>
      )}
      {selectedError && <ErrorMessage>Cannot load platform experiment {peID}: {selectedError.message}</ErrorMessage>}
      {error && <ErrorMessage>Cannot load the hypothesis graph: {error.message}</ErrorMessage>}
      {(peID ? !(graph && pe) && !error && !selectedError : !platformExperiments && !peError) && <Loading />}

      {graph && positions && pe && <ObjectiveBar graph={graph} pe={pe} onAddIdea={setAddUnder} />}
      {graph && positions && pe && (
        <div className={clsx('grid min-h-[420px] flex-[1_0_0] grid-rows-[minmax(0,1fr)] gap-4', showSide ? 'grid-cols-[minmax(0,1fr)_340px]' : 'grid-cols-[minmax(0,1fr)]')}>
          <div className="flex flex-col overflow-hidden rounded-lg border bg-surface">
            {graph.nodes.length === 0 ? (
              <EmptyState>
                No hypotheses in this experiment yet. Agents place each idea under the objective or an
                earlier hypothesis{example ? '.' : '; you can add one under the objective above.'}
              </EmptyState>
            ) : view === 'outline' ? (
              <HypothesisOutline
                graph={graph}
                rootLabel={pe.name}
                evidence={evidence}
                selected={selected}
                hits={hits}
                filters={filters}
                onSelect={setFocus}
              />
            ) : (
              <HypothesisGraphCanvas
                key={graph.root}
                graph={graph}
                positions={positions}
                rootLabel={pe.name}
                evidence={evidence}
                focus={focus}
                selected={selected}
                hits={hits}
                filters={filters}
                minimap={!showSide}
                onSelect={setFocus}
              />
            )}
          </div>
          {showSide && <aside className="overflow-y-auto">
            {q && (
              <Card>
                <CardHeader>
                  Search results
                  <Button variant="link" size="sm" className="ml-auto" onClick={() => setQ('')}>Clear</Button>
                </CardHeader>
                <CardContent>
                  <SearchHits hits={search.data} onPick={h => setFocus(h.id)} isLoading={search.isLoading} error={search.error} />
                </CardContent>
              </Card>
            )}
            {selected !== graph.root && (
              <HypothesisPanel
                graph={graph}
                nodeByID={nodeByID}
                evidence={evidence}
                selected={selected}
                onSelect={setFocus}
                onAddIdea={setAddUnder}
              />
            )}
          </aside>}
        </div>
      )}

      {graph && pe && !example && addUnder !== null && (
        <Dialog open onClose={() => setAddUnder(null)} title="Add an idea">
          <AddIdeaForm
            platformExperimentID={peID}
            parent={{
              id: addUnder,
              label: addUnder === graph.root ? `the objective of ${pe.name}` : nodeByID.get(addUnder)!.text,
            }}
            onAdded={onAdded}
          />
        </Dialog>
      )}
    </div>
  )
}

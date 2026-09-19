'use client'

import { Suspense, useEffect, useMemo, useState } from 'react'
import { useRouter, useSearchParams } from 'next/navigation'
import useSWR from 'swr'
import { Dialog, DialogPanel, DialogTitle } from '@headlessui/react'
import { fetchHypothesisGraph, fetchPlatformExperiments } from '@/lib/api'
import type { PlatformExperiment } from '@/types'
import { PageHeader } from '@/components/ui/page-header'
import { Pod, PodHeader, PodContent } from '@/components/ui/pod'
import { Button, Chip } from '@/components/ui/button'
import { Loading, ErrorMessage, EmptyState } from '@/components/ui/status-message'
import { SearchableSelect } from '@/components/ui/searchable-select'
import { AddIdeaForm } from '@/components/human-idea'
import { HypothesisGraphCanvas } from '@/components/hypothesis-graph'
import { HypothesisOutline } from '@/components/hypothesis-outline'
import { HypothesisPanel } from '@/components/hypothesis-panel'
import { SearchBox, SearchHits, useHypothesisSearch } from '@/components/hypothesis-search'
import { useQueryParam } from '@/lib/query-state'
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
  const router = useRouter()
  const searchParams = useSearchParams()
  const peID = searchParams.get('pe') ?? ''
  const [focus, setFocus] = useQueryParam('focus')
  const [view, setView] = useQueryParam('view', 'graph')
  const [statusFilter, setStatusFilter] = useQueryParam('status')
  const [ownerFilter, setOwnerFilter] = useQueryParam('agent')
  const [q, setQ] = useQueryParam('q')
  const [addUnder, setAddUnder] = useState<string | null>(null)

  const { data: platformExperiments, error: peError } = useSWR<PlatformExperiment[]>(
    'platform-experiments-all',
    () => fetchPlatformExperiments(),
    { refreshInterval: 30_000 },
  )
  const sortedPEs = useMemo(
    () => [...(platformExperiments ?? [])].sort((a, b) => b.created_at.localeCompare(a.created_at)),
    [platformExperiments],
  )
  const pe = sortedPEs.find(p => p.id === peID)

  // A graph belongs to one experiment, so the URL always names one: the newest by default.
  useEffect(() => {
    if (!peID && sortedPEs.length > 0) router.replace(graphHref(sortedPEs[0].id))
  }, [peID, sortedPEs, router])

  const { data: graph, error, isLoading, mutate } = useSWR(
    peID ? ['hypothesis-graph', peID] : null,
    () => fetchHypothesisGraph(peID),
    { refreshInterval: 15_000 },
  )
  const search = useHypothesisSearch(peID, q)

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
  const rootLabel = pe?.name ?? peID
  const filtering = statusFilter !== '' || ownerFilter !== ''
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
    <div>
      <PageHeader
        title="Hypotheses"
        description="Each platform experiment grows a graph of research claims. Every hypothesis builds on the experiment's objective or on earlier hypotheses, and may contradict or relate to others. Registering text equivalent to an existing hypothesis in the same experiment returns the existing one."
        actions={<Button size="sm" onClick={() => mutate()}>Refresh</Button>}
      />

      <div style={{ display: 'flex', alignItems: 'center', gap: 10, marginBottom: 10, flexWrap: 'wrap' }}>
        <span className="uppercase-label">Platform Experiment</span>
        <SearchableSelect
          value={peID}
          onChange={selectExperiment}
          hideAllOption
          placeholder="Select an experiment"
          style={{ minWidth: 260 }}
          options={sortedPEs.map(p => ({ value: p.id, label: p.name }))}
        />
        <div style={{ display: 'flex', gap: 6, marginLeft: 'auto' }} role="group" aria-label="View">
          <Chip active={view === 'graph'} onClick={() => setView('graph')}>Graph</Chip>
          <Chip active={view === 'outline'} onClick={() => setView('outline')}>Outline</Chip>
        </div>
      </div>

      <div style={{ display: 'flex', alignItems: 'center', gap: 10, marginBottom: 12, flexWrap: 'wrap' }}>
        <SearchBox key={peID} initial={q} placeholder="Search hypotheses by text" onSearch={setQ} />
        <span className="uppercase-label" style={{ marginLeft: 8 }}>Status</span>
        <SearchableSelect
          value={statusFilter}
          onChange={setStatusFilter}
          allLabel="All"
          options={STATUS_OPTIONS}
        />
        <span className="uppercase-label" style={{ marginLeft: 8 }}>Agent</span>
        <SearchableSelect
          value={ownerFilter}
          onChange={setOwnerFilter}
          allLabel="All agents"
          options={ownerOptions}
        />
        {filtering && graph && (
          <span className="text-dim">{matching} of {graph.nodes.length} match</span>
        )}
      </div>

      {peError && <ErrorMessage>Cannot reach registry service.</ErrorMessage>}
      {platformExperiments && sortedPEs.length === 0 && (
        <EmptyState>No platform experiments yet. A hypothesis graph grows inside one.</EmptyState>
      )}
      {error && <ErrorMessage>Cannot load the hypothesis graph: {error.message}</ErrorMessage>}
      {(isLoading || (!peID && !platformExperiments && !peError)) && <Loading />}

      {graph && positions && (
        <div className="hg-layout">
          <div className="hg-main">
            {graph.nodes.length === 0 ? (
              <EmptyState>
                No hypotheses in this experiment yet. Agents place each idea under the objective or an
                earlier hypothesis; you can add one under the objective from the panel.
              </EmptyState>
            ) : view === 'outline' ? (
              <HypothesisOutline
                graph={graph}
                rootLabel={rootLabel}
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
                rootLabel={rootLabel}
                focus={focus}
                selected={selected}
                hits={hits}
                filters={filters}
                onSelect={setFocus}
              />
            )}
          </div>
          <aside className="hg-side">
            {q && (
              <Pod>
                <PodHeader>
                  Search results
                  <Button variant="link" style={{ marginLeft: 'auto', fontSize: 11.5 }} onClick={() => setQ('')}>Clear</Button>
                </PodHeader>
                <PodContent>
                  <SearchHits hits={search.data} onPick={h => setFocus(h.id)} isLoading={search.isLoading} error={search.error} />
                </PodContent>
              </Pod>
            )}
            <HypothesisPanel
              graph={graph}
              nodeByID={nodeByID}
              pe={pe}
              selected={selected}
              onSelect={setFocus}
              onAddIdea={setAddUnder}
            />
          </aside>
        </div>
      )}

      {graph && addUnder !== null && (
        <Dialog open onClose={() => setAddUnder(null)} className="wa-dialog">
          <div className="wa-dialog-backdrop" aria-hidden="true" />
          <div className="wa-dialog-container">
            <DialogPanel className="wa-dialog-panel">
              <DialogTitle style={{ fontWeight: 600, marginBottom: 10 }}>Add an idea</DialogTitle>
              <AddIdeaForm
                platformExperimentID={peID}
                parent={{
                  id: addUnder,
                  label: addUnder === graph.root ? `the objective of ${rootLabel}` : nodeByID.get(addUnder)!.text,
                }}
                onAdded={onAdded}
              />
            </DialogPanel>
          </div>
        </Dialog>
      )}
    </div>
  )
}

'use client'

import { createContext, useContext, useMemo } from 'react'
import type { ReactNode } from 'react'
import useSWR from 'swr'
import { fetchPlatformExperimentAgents } from '@/lib/api'

const Names = createContext<Map<string, string>>(new Map())

/** The names of a platform experiment's agents, for everything under it that shows an agent. */
export function AgentNames({ platformExperimentID, children }: { platformExperimentID: string; children: ReactNode }) {
  // The agents card reads the same key, so this is no request of its own.
  const { data } = useSWR(['pe-agents', platformExperimentID], () => fetchPlatformExperimentAgents(platformExperimentID), { refreshInterval: 15_000 })
  const names = useMemo(() => new Map((data ?? []).map(a => [a.agent_id, a.name])), [data])
  return <Names.Provider value={names}>{children}</Names.Provider>
}

/** What to call an agent: its name, or its id while the names are unread or for one that has left. */
export function useAgentName(): (id: string) => string {
  const names = useContext(Names)
  return id => names.get(id) || id
}

/** An agent by name, with its id on hover. */
export function AgentLabel({ id }: { id: string }) {
  const name = useAgentName()(id)
  return <span title={name === id ? undefined : id}>{name}</span>
}

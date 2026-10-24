'use client'

import { createContext, useContext, useMemo } from 'react'
import type { ReactNode } from 'react'
import useSWR from 'swr'
import { fetchPlatformExperimentAgents } from '@/lib/api/agents'
import { keys } from '@/lib/keys'
import { shortId } from '@/lib/format'

const Names = createContext<{ names: Map<string, string>; error?: Error }>({ names: new Map() })

/** The names of a platform experiment's agents, for everything under it that shows an agent. */
export function AgentNames({ platformExperimentID, children }: { platformExperimentID: string; children: ReactNode }) {
  // The agents card reads the same key, so this is no request of its own.
  const { data, error } = useSWR(keys.roster(platformExperimentID), () => fetchPlatformExperimentAgents(platformExperimentID))
  const value = useMemo(() => ({ names: new Map((data ?? []).map(a => [a.agent_id, a.name])), error }), [data, error])
  return <Names.Provider value={value}>{children}</Names.Provider>
}

/** What to call an agent: its name, or its id while the names are unread or for one that has left. */
export function useAgentName(): (id: string) => string {
  const { names } = useContext(Names)
  return id => names.get(id) || id
}

/** An agent by name, with its id on hover; by its id, marked, when the names could not be read. */
export function AgentLabel({ id }: { id: string }) {
  const { names, error } = useContext(Names)
  const name = names.get(id)
  if (name) return <span title={id}>{name}</span>
  if (error) return <span className="cursor-help font-mono text-danger" title={`${id}: could not load the agents' names, ${error.message}`}>{shortId(id)}</span>
  return <span title={id}>{shortId(id)}</span>
}

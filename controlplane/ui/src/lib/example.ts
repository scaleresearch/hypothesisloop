'use client'

import { createContext, useContext } from 'react'
import { exampleHref, useRecordExample, useRecordProject } from '@/lib/query-state'
import { useTenant } from '@/lib/tenant'
import type { Me, PlatformExperiment } from '@/types'

/** The example the pages are inside. `pe` is absent until it has been read. */
export interface Example {
  id: string
  pe?: PlatformExperiment
}

// Provided once, by the signed-in shell, from the example the URL names. Everything that writes
// to a platform experiment or to a record in it asks useReadOnly and leaves its control out.
export const ExampleContext = createContext<Example | null>(null)

export function useExample(): Example | null {
  return useContext(ExampleContext)
}

/** Whether the pages show an example: the API refuses every write to one. */
export function useReadOnly(): boolean {
  return useExample() !== null
}

function reaches(me: Me, projectID: string): boolean {
  return me.projects.some(p => p.id === projectID)
}

/** A platform experiment the caller only reads: staff shared it, and its project is not one of theirs. */
export function isExample(me: Me, pe: Pick<PlatformExperiment, 'shared' | 'project_id'>): boolean {
  return pe.shared && !reaches(me, pe.project_id)
}

/** Where a platform experiment opens for this caller: as an example when that is what it is to them. */
export function platformExperimentHref(me: Me, pe: Pick<PlatformExperiment, 'id' | 'shared' | 'project_id'>): string {
  return isExample(me, pe) ? exampleHref(pe.id) : `/platform-experiments/${pe.id}`
}

/**
 * The scope a list is asked for: the selected project, or inside an example that platform
 * experiment alone. The example lies in another org's project, so the caller's project_id
 * would match nothing in it.
 */
export function useListScope(): { project_id: string } | { platform_experiment_id: string } {
  const { project } = useTenant()
  const example = useExample()
  return example ? { platform_experiment_id: example.id } : { project_id: project.id }
}

/**
 * Puts where a loaded record lives in the URL: its project when the caller reaches it, and
 * otherwise the example it belongs to, which is the only way they came to read it. Answers
 * false while a record of an example waits for the URL to name it: until then the page would
 * show the controls of an experiment of the caller's own.
 */
export function useRecordScope(record: { project_id: string; platform_experiment_id: string } | undefined): boolean {
  const { me } = useTenant()
  const own = record !== undefined && reaches(me, record.project_id)
  useRecordProject(own ? record.project_id : undefined)
  const named = useRecordExample(record && !own ? record.platform_experiment_id : undefined)
  return record === undefined || own || named
}

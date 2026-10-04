'use client'

import { createContext, useContext } from 'react'
import useSWR from 'swr'
import { fetchPlatformExperiment } from '@/lib/api'
import { useTenant } from '@/lib/tenant'
import type { Me, PlatformExperiment, Project } from '@/types'

function reaches(me: Me, projectID: string): boolean {
  return me.projects.some(p => p.id === projectID)
}

/**
 * A platform experiment the caller only reads: staff shared it with every account, and its
 * project is not one of the caller's. The API refuses them every write to it.
 */
export function isReadOnly(me: Me, pe: Pick<PlatformExperiment, 'shared' | 'project_id'>): boolean {
  return pe.shared && !reaches(me, pe.project_id)
}

// Set by a page from the platform experiment its records belong to. Everything that writes to
// a platform experiment or to a record in it asks useReadOnly and leaves its control out.
const ReadOnlyContext = createContext(false)

export const ReadOnlyProvider = ReadOnlyContext.Provider

export function useReadOnly(): boolean {
  return useContext(ReadOnlyContext)
}

export interface ExperimentAccess {
  pe?: PlatformExperiment
  readOnly: boolean
  /** False while the platform experiment is being read. */
  known: boolean
}

/**
 * What the caller may do in the platform experiment a page is about, read from the experiment
 * itself. Until it has been read nothing can be written, so no control shows and goes again.
 * With no id the page is about the caller's own project.
 */
export function useExperimentAccess(platformExperimentID: string | undefined): ExperimentAccess {
  const { me } = useTenant()
  const { data: pe, error } = useSWR(
    platformExperimentID ? ['pe', platformExperimentID] : null,
    () => fetchPlatformExperiment(platformExperimentID as string),
  )
  if (!platformExperimentID) return { readOnly: false, known: true }
  return { pe, readOnly: pe ? isReadOnly(me, pe) : true, known: pe !== undefined || error !== undefined }
}

/**
 * The project a list narrowed to that platform experiment is asked within: the selected one,
 * and none for a shared experiment of another org, which lies in a project the caller's
 * project_id would never match.
 */
export function listProject(project: Project, access: ExperimentAccess): { project_id?: string } {
  return access.pe && access.readOnly ? {} : { project_id: project.id }
}

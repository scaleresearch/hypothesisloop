'use client'

import { createContext, useContext } from 'react'
import useSWR from 'swr'
import { fetchPlatformExperiment } from '@/lib/api/platform-experiments'
import { keys } from '@/lib/keys'
import type { PlatformExperiment, PlatformExperimentAction, Project } from '@/types/api'

// Set by a page from the platform experiment its records belong to, as the API answered it.
// Outside one nothing is offered.
const ExperimentActionsContext = createContext<PlatformExperimentAction[]>([])

export const ExperimentActionsProvider = ExperimentActionsContext.Provider

export function useExperimentActions(): PlatformExperimentAction[] {
  return useContext(ExperimentActionsContext)
}

interface ExperimentAccess {
  pe?: PlatformExperiment
  /** False while the platform experiment is being read. */
  known: boolean
}

/** The platform experiment a page is about, read for what it allows. With no id there is none. */
export function useExperimentAccess(platformExperimentID: string | undefined): ExperimentAccess {
  const { data: pe, error } = useSWR(
    platformExperimentID ? keys.experiment(platformExperimentID) : null,
    () => fetchPlatformExperiment(platformExperimentID as string),
  )
  if (!platformExperimentID) return { known: true }
  return { pe, known: pe !== undefined || error !== undefined }
}

/**
 * The project a list narrowed to that platform experiment is asked within: the selected one, and
 * none for an experiment of another project, which the selected project_id would never match.
 */
export function listProject(project: Project, access: ExperimentAccess): { project_id?: string } {
  return access.pe && access.pe.project_id !== project.id ? {} : { project_id: project.id }
}

import type { PlatformExperiment, PlatformExperimentPatch } from '@/types/api'

/** A field of PATCH /platform-experiments/{id}. */
export type EditableField = keyof PlatformExperimentPatch

/** Whether the reader may change field of pe now, as the API says in editable. */
export function canEdit(pe: Pick<PlatformExperiment, 'editable'>, field: EditableField): boolean {
  return pe.editable.includes(field)
}

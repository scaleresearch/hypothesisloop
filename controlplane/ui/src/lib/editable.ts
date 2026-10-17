import type { PlatformExperiment } from '@/types/api'

/** A field of PATCH /platform-experiments/{id}. */
export type EditableField = 'goal' | 'name' | 'description' | 'budget_accelerator_hours' | 'max_agents' | 'researchers' |
  'max_concurrent_accelerators' | 'metrics' | 'report_interval_seconds' | 'starts_at' | 'duration_hours' | 'baseline_job_id' | 'baseline_reason'

/** Whether the reader may change field of pe now, as the API says in editable. */
export function canEdit(pe: Pick<PlatformExperiment, 'editable'>, field: EditableField): boolean {
  return pe.editable?.includes(field) ?? false
}

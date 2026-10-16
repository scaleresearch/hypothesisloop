
/** A new experiment's form as a person left it in a project. The API stores `form` unread. */
export interface ExperimentDraft {
  id: string
  project_id: string
  form: Record<string, unknown>
  step: string
  /** Why the last create from it was refused, empty when none was. */
  failure: string
  created_at: string
  updated_at: string
}

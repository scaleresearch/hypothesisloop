import { apiFetch } from '@/lib/api/request'
import type { Model, ModelListing } from '@/types/api'

/** A model the platform serves, priced in USD per million tokens. */
export type ListedModel = Model

export type ModelProvider = Model['provider']

export type { ModelListing }

// Every model of every provider the platform serves: the only ones an agent or coordinator may run on.
export function fetchModels(): Promise<ModelListing> {
  return apiFetch<ModelListing>('/models')
}

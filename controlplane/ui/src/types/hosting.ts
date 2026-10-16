import type { StatusReason } from '@/types/platform-experiments'

// Whether the platform can host agents (GET /hosting): exactly when its settings have a sandbox
// provider. unavailable says why while it cannot: reason hosting_not_configured.
export interface Hosting {
  available: boolean
  unavailable?: StatusReason
}

import type { StatusReason } from '@/types/platform-experiments'

// Whether the platform can host agents now (GET /hosting). unavailable says why while it cannot:
// reason launcher_not_registered or launcher_unreachable.
export interface Hosting {
  available: boolean
  unavailable?: StatusReason
}

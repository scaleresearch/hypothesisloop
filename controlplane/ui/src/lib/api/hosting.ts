import { apiFetch } from '@/lib/api/request'
import type { Hosting } from '@/types/hosting'

// Whether the platform can host agents now, and why not while it cannot.
export function fetchHosting(): Promise<Hosting> {
  return apiFetch<Hosting>('/hosting')
}

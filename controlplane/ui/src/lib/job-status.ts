// What a job's status reads as. The platform has no status for a cancel: a job cancelled before
// it ran is REJECTED and one cancelled while running is EVICTED, each with eviction_reason
// cancelled, and neither word says what happened.
export function jobStatusLabel(job: { status?: string | null; eviction_reason?: string | null }): string {
  return job.eviction_reason === 'cancelled' ? 'CANCELLED' : job.status ?? 'UNKNOWN'
}

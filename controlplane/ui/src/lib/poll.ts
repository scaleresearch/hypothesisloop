/** How often a page reads again what the change stream does not carry, by how fast that changes. */
export const POLL = {
  /** Which clusters are connected and what they have free. */
  live: 5_000,
  /** What a job reports while it runs, the clusters jobs are placed on, and what an experiment spent. */
  reports: 10_000,
  /** Standings and quotas, which follow what jobs report and spend, the platform's job counts, and whether agent sandboxes are available. */
  standing: 15_000,
  /** The account, metric histories and the quotas of an agent. */
  slow: 30_000,
} as const

/** How often a page reads again what it shows, by how fast that changes. */
export const POLL = {
  /** A planner at work, whose own sentence of what it does is all there is to watch. */
  planning: 3_000,
  /** A job's status, and which clusters are connected and what they have free. */
  live: 5_000,
  /** The jobs of a list and a hypothesis's evidence, which agents add to while one watches. */
  activity: 8_000,
  /** What a running job reports, the clusters jobs are placed on, and a project's experiments with what they spent. */
  reports: 10_000,
  /** Counts, standings, rosters, graphs and lists of experiments and agents, and whether hosting is available. */
  standing: 15_000,
  /** The account, metric histories, quotas and agents' totals. */
  slow: 30_000,
} as const

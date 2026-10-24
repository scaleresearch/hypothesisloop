/**
 * The SWR key of every read. A change revalidates through these same builders, so it reaches
 * exactly what was read. The first element of each key names what it reads.
 */
export const keys = {
  me: () => ['me'],
  agentSandbox: () => ['agentsandbox'],
  models: () => ['models'],
  agentRoles: () => ['agent-roles'],

  apiKeys: (orgID: string) => ['api-keys', orgID],
  orgMembers: (orgID: string) => ['org-members', orgID],
  orgInvites: (orgID: string) => ['org-invites', orgID],

  agentTypes: (orgID: string) => ['agent-types', orgID],
  agentType: (orgID: string, typeID: string) => ['agent-type', orgID, typeID],
  agentTypeVersions: (orgID: string, typeID: string, version: number) => ['agent-type-versions', orgID, typeID, version],

  allClusters: () => ['all-clusters'],
  orgClusters: (orgID: string) => ['org-clusters', orgID],
  resourceCapacity: (orgID: string) => ['resource-capacity', orgID],

  githubApp: () => ['github-app'],
  githubInstallations: (orgID: string) => ['github-installations', orgID],
  githubInstallationCheck: (orgID: string, installationID: number) => ['github-installation-check', orgID, installationID],
  githubInstallationChecks: (orgID: string, installationIDs: number[]) => ['github-installation-checks', orgID, ...installationIDs],
  githubRepositories: (orgID: string, q: string) => ['github-repositories', orgID, q],
  repositoryCheck: (projectID: string, repository: string) => ['repository-check', projectID, repository],

  experiment: (id: string) => ['pe', id],
  experiments: (projectID: string, ...filters: unknown[]) => ['platform-experiments', projectID, ...filters],
  runningExperiments: (projectID: string) => ['platform-experiments-running', projectID],
  experimentSearch: (projectID: string, q: string) => ['platform-experiment-search', projectID, q],
  experimentDrafts: (projectID: string) => ['experiment-drafts', projectID],
  roster: (id: string) => ['pe-agents', id],
  messages: (id: string) => ['pe-messages', id],
  quotas: (id: string) => ['pe-quotas', id],
  agentQuota: (id: string, agentID: string) => ['agent-quota', id, agentID],
  stages: (id: string) => ['pe-stages', id],
  results: (id: string) => ['pe-results', id],
  findings: (id: string) => ['pe-findings', id],
  hypothesisStats: (id: string) => ['pe-hypothesis-stats', id],
  metricSeries: (id: string, ...window: unknown[]) => ['pe-all-metrics', id, ...window],

  hypothesis: (id: string) => ['hypothesis', id],
  hypothesisGraph: (experimentID: string) => ['hypothesis-graph', experimentID],
  hypothesisSearch: (experimentID: string, q: string) => ['hypothesis-search', experimentID, q],

  job: (id: string) => ['job', id],
  jobLogs: (id: string) => ['job-logs', id],
  jobMetrics: (id: string) => ['job-metrics', id],
  jobs: (scope: string, ...filters: unknown[]) => ['jobs', scope, ...filters],
  jobListStats: (scope: string, ...filters: unknown[]) => ['job-stats', scope, ...filters],
  experimentJobs: (id: string) => ['pe-experiments', id],
  runningJobs: (id: string) => ['pe-running', id],
  agentJobs: (id: string, agentID: string) => ['pe-agent-jobs', id, agentID],
  jobStatsByAgent: (id: string) => ['pe-stats-by-agent', id],
  jobStatsByHypothesis: (id: string) => ['pe-stats-by-hypothesis', id],
  projectJobStats: (projectID: string) => ['experiment-stats', projectID],
  allJobStats: () => ['all-experiment-stats'],
  recentEvictions: (projectID: string) => ['recent-evictions', projectID],

  agents: (projectID: string, ...filters: unknown[]) => ['agents-page', projectID, ...filters],
  agentStats: (projectID: string, ...filters: unknown[]) => ['agent-stats', projectID, ...filters],
  agentCount: (projectID: string) => ['agent-count', projectID],
  agentSearch: (scope: unknown, q: string) => ['agent-search', scope, q],
}

function ofKinds(...kinds: string[]) {
  const set = new Set(kinds)
  return (key: unknown) => Array.isArray(key) && set.has(key[0])
}

/** The reads a removed experiment no longer counts in. */
export const experimentTallies = ofKinds('platform-experiments', 'experiment-stats', 'all-experiment-stats', 'agent-count')

/** The reads of what the org's GitHub accounts reach, which an install changes. */
export const githubReads = ofKinds('github-installations', 'github-repositories', 'repository-check', 'github-installation-check', 'github-installation-checks')

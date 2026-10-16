import { apiCall, apiFetch, apiSend } from '@/lib/api/request'
import type { Hypothesis, HypothesisComment, HypothesisEvidence, HypothesisGraph, HypothesisLink, HypothesisLinkInput, HypothesisLinkType, HypothesisSearchHit, HypothesisStats, HypothesisStatus, HypothesisWithJobs, RegisteredHypothesis } from '@/types/hypotheses'

export function fetchHypothesisStats(platformExperimentID: string): Promise<HypothesisStats> {
  return apiFetch<HypothesisStats>(
    `/hypotheses/stats?platform_experiment_id=${encodeURIComponent(platformExperimentID)}`,
  )
}

// Drawn from the 200 newest hypotheses.
export function fetchFindings(platformExperimentID: string): Promise<HypothesisEvidence[]> {
  return apiFetch<HypothesisEvidence[]>(`/platform-experiments/${platformExperimentID}/findings`)
}

export function fetchHypothesis(id: string): Promise<HypothesisWithJobs> {
  return apiFetch<HypothesisWithJobs>(`/hypotheses/${id}`)
}

export function fetchHypothesisGraph(platformExperimentID: string): Promise<HypothesisGraph> {
  return apiFetch<HypothesisGraph>(
    `/hypotheses/graph?platform_experiment_id=${encodeURIComponent(platformExperimentID)}`,
  )
}

interface HypothesisQueryResult {
  columns: string[]
  rows: unknown[][]
  truncated: boolean
}

// Read-only Cypher against one platform experiment's hypothesis graph, which the backend binds as
// $pe. A Cypher error comes back as a 400 whose body is the database's own message.
function queryHypothesisGraph(platformExperimentID: string, cypher: string, params: Record<string, unknown>): Promise<HypothesisQueryResult> {
  return apiSend<HypothesisQueryResult>('POST', '/hypotheses/query', { platform_experiment_id: platformExperimentID, cypher, params })
}

// The text index name and call syntax are defined by the backend in the POST /hypotheses/query
// operation description (served at /explore); keep this in step with it.
const HYPOTHESIS_SEARCH_CYPHER = `
CALL text_search.search_all('hypothesis_text', $q) YIELD node, score
WHERE node.platform_experiment_id = $pe
RETURN node.id AS id, node.text AS text, node.status AS status
ORDER BY score DESC
LIMIT 20`

// The index parses its argument as a query: quotes, colons and brackets are syntax and uppercase
// AND/OR/NOT are operators, so typed text is passed on as lowercase words only. A literal /u regex
// needs an es6 target, which this tsconfig is not.
const NON_WORD = new RegExp('[^\\p{L}\\p{N}]+', 'gu')

export async function searchHypotheses(platformExperimentID: string, q: string): Promise<HypothesisSearchHit[]> {
  const words = q.toLowerCase().replace(NON_WORD, ' ').trim()
  if (!words) return []
  const result = await queryHypothesisGraph(platformExperimentID, HYPOTHESIS_SEARCH_CYPHER, { q: words })
  return result.rows.map(([id, text, status]) => ({
    id: id as string, text: text as string, status: status as HypothesisStatus,
  }))
}

// Registers a human's idea, placed in the graph by `links` (at least one builds_on; `to` is a
// hypothesis id, or the platform_experiment_id for the objective root). Who registered it is the
// signed-in person, read by the API from their session. Text equivalent to an existing claim returns
// that row with already_existed set, and the supplied links are not applied. Text close to other
// hypotheses is registered all the same, and those come back in similar.
export function registerHumanHypothesis(
  platformExperimentID: string, text: string, links: HypothesisLinkInput[],
): Promise<RegisteredHypothesis> {
  return apiSend<RegisteredHypothesis>('POST', '/hypotheses', { platform_experiment_id: platformExperimentID, text, links })
}

// Links are append-only. Only a hypothesis no agent owns accepts links from a person as themselves.
// Answers the hypothesis's outgoing links.
export function addHumanHypothesisLink(
  hypothesisID: string, type: HypothesisLinkType, to: string,
): Promise<HypothesisLink[]> {
  return apiSend<HypothesisLink[]>('POST', `/hypotheses/${hypothesisID}/links`, { type, to })
}

// The author edits their own idea, and the author or an org owner removes it, while no agent has
// acted on it and the experiment is open.
export function editHumanHypothesis(hypothesisID: string, text: string): Promise<Hypothesis> {
  return apiSend<Hypothesis>('PATCH', `/hypotheses/${hypothesisID}`, { text })
}

export async function removeHumanHypothesis(hypothesisID: string): Promise<void> {
  await apiCall('DELETE', `/hypotheses/${hypothesisID}`)
}

export function addHumanHypothesisComment(hypothesisID: string, text: string): Promise<HypothesisComment> {
  return apiSend<HypothesisComment>('POST', `/hypotheses/${hypothesisID}/comments`, { text })
}

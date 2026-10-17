import { apiFetch, apiURL, withQuery } from '@/lib/api/request'

/** One change of GET /watch and GET /changes, a pointer to what to read again. Mirrors domain.Event. */
export interface Change {
  id: number
  kind: string
  subject: string
  value: string
  detail: string
  platform_experiment_id: string
  agent_id: string
  occurred_at: string
}

/** Where a read of the changes starts: after the id of the last change read, or at the newest. */
export type After = number | 'head'

/** The subprotocol GET /watch speaks. The token is offered beside it, since a browser sets no header on a socket. */
export const WATCH_SUBPROTOCOL = 'hypothesisloop.watch'

function changesQuery(projectID: string, kinds: readonly string[], after: After) {
  return { project_id: projectID, kinds: kinds.join(','), after }
}

/** One read of the project's changes after after: what the socket would stream, answered with the API's status. */
export function fetchChanges(projectID: string, kinds: readonly string[], after: After): Promise<unknown> {
  return apiFetch(withQuery('/changes', changesQuery(projectID, kinds, after)))
}

/** The address of the socket that streams the project's changes after after. */
export function watchURL(projectID: string, kinds: readonly string[], after: After): string {
  const url = new URL(`${apiURL()}${withQuery('/watch', changesQuery(projectID, kinds, after))}`)
  url.protocol = url.protocol === 'https:' ? 'wss:' : 'ws:'
  return url.toString()
}

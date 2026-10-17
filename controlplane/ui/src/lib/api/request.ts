import { accessToken, endSession } from '@/lib/auth'
import { publicConfig } from '@/lib/config'
import type { ErrorModel } from '@/types/api'

// One API, one base URL: quota, scheduler and registry operations are all served together.
export function apiURL(): string {
  const url = publicConfig.apiURL()
  if (!url) throw new Error('UI_API_URL is not set, as .env.example shows')
  return url
}

/** A refusal or failure the API answered with: its status, its own message, and its reason code, which only an answer in place of the API's lacks. */
export class ApiError extends Error {
  /** `body` is the whole answer, for a refusal that names its problems. */
  constructor(readonly status: number, message: string, readonly reason?: string, readonly body?: ErrorModel) {
    super(message)
    this.name = 'ApiError'
  }
}

// The API answers every failure as ErrorModel.
async function failure(res: Response): Promise<ApiError> {
  const text = await res.text()
  let message = text
  let reason: string | undefined
  let body: ErrorModel | undefined
  try {
    body = JSON.parse(text) as ErrorModel
    message = body.message
    reason = body.reason
  } catch {
    // A proxy in front of the API answers in its own format.
  }
  return new ApiError(res.status, message || `API error ${res.status}`, reason, body)
}

// Every call to the API goes through here: it carries the session's access token, and a session
// that is gone or that the API refuses has one outcome, the sign-in page.
async function request(method: string, path: string, body?: unknown): Promise<Response> {
  const token = await accessToken()
  if (!token) return endSession('ended')
  const res = await fetch(`${apiURL()}${path}`, {
    method,
    cache: 'no-store',
    headers: { Authorization: `Bearer ${token}`, ...(body === undefined ? {} : { 'Content-Type': 'application/json' }) },
    body: body === undefined ? undefined : JSON.stringify(body),
  })
  if (res.status === 401) return endSession('refused')
  return res
}

export async function apiCall(method: string, path: string, body?: unknown): Promise<Response> {
  const res = await request(method, path, body)
  if (!res.ok) throw await failure(res)
  return res
}

export async function apiSend<T>(method: string, path: string, body?: unknown): Promise<T> {
  return (await apiCall(method, path, body)).json() as Promise<T>
}

export function apiFetch<T>(path: string): Promise<T> {
  return apiSend<T>('GET', path)
}

// Query params are the fields of `params` sent under their own names, unset ones left out.
export function withQuery(path: string, params: object): string {
  const query = new URLSearchParams()
  for (const [k, v] of Object.entries(params)) {
    if (v !== undefined && v !== '') query.set(k, String(v))
  }
  return query.size ? `${path}?${query}` : path
}

// A paginated list response: the page of items plus the total match count across all pages,
// read off the X-Total-Count response header, as every list endpoint sends it (?limit, ?offset,
// ?sort; total in X-Total-Count).
export interface Page<T> {
  items: T[]
  total: number
}

export const MAX_LIST_PAGE_SIZE = 200

export async function apiFetchPage<T>(path: string): Promise<Page<T>> {
  const res = await apiCall('GET', path)
  const total = res.headers.get('X-Total-Count')
  if (total == null) throw new Error(`${path} answered a list without X-Total-Count`)
  return { items: (await res.json()) as T[], total: Number(total) }
}

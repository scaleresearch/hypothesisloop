import { ApiError } from '@/lib/api/request'

/** The words of anything thrown, for a person to read. */
export function errorMessage(e: unknown): string {
  return e instanceof Error ? e.message : String(e)
}

/**
 * The problem with the text sent on its field after the field its message leads with: the API
 * never repeats a value sent, so "agents[1].model is not served: ..." reads
 * "agents[1].model openrouter/x/y is not served: ..." here.
 */
export function namingSent<P extends { field?: string; message: string }>(problem: P, sent: unknown): P {
  const { field, message } = problem
  if (!field || !message.startsWith(`${field} `)) return problem
  const value = field.split(/[.[\]]+/).filter(Boolean).reduce<unknown>((at, key) => (at as Record<string, unknown> | undefined)?.[key], sent)
  return typeof value === 'string' && value ? { ...problem, message: `${field} ${value}${message.slice(field.length)}` } : problem
}

/** The words of anything thrown, each problem of a refusal naming the text sent on its field. */
export function refusalMessage(e: unknown, sent: unknown): string {
  const problems = e instanceof ApiError ? e.body?.problems : undefined
  if (!problems?.length) return errorMessage(e)
  return problems.map(p => namingSent(p, sent).message).join(' ')
}

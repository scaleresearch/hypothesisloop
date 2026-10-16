/** The words of anything thrown, for a person to read. */
export function errorMessage(e: unknown): string {
  return e instanceof Error ? e.message : String(e)
}

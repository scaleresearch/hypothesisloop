import { readFileSync } from 'node:fs'
import { dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'

const repo = join(dirname(fileURLToPath(import.meta.url)), '..', '..', '..')

/** The settings the running control plane was started with, which hl-admin reads too. */
export const SETTINGS = join(repo, 'controlplane', 'settings', '.hypothesisloop.dev.yaml')

function setting(key) {
  const text = readFileSync(SETTINGS, 'utf8')
  const value = text.match(new RegExp(`^\\s+${key}:\\s*(\\S+)\\s*$`, 'm'))?.[1]
  if (!value) throw new Error(`${SETTINGS} names no ${key}: render it with make dev`)
  return value
}

/** The one browser origin the API allows, which the GitHub App returns to. */
export const UI = setting('ui_origin')
/** The API of the stack make dev runs on this machine. */
export const API = `http://localhost:${setting('api_port')}`

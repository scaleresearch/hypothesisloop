import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'
import { describe, expect, it } from 'vitest'
import { BARE_METAL_SCRIPT, KUBERNETES_SCRIPT, startCommand } from './attach-command'

// The CLI prints the same commands from its own copy of the two scripts (cli/accounts.go). A
// script changed in one place and not the other would have the page and hl disagree on how a
// cluster is attached, so the Go constants are read here and held equal.
const cli = readFileSync(resolve(__dirname, '../../../../cli/accounts.go'), 'utf8')

function goConstant(name: string): string {
  const declared = cli.match(new RegExp(`\\nconst ${name} = ((?:\`[^\`]*\`(?: \\+\\n\\t)?)+)\\n`))
  if (!declared) throw new Error(`cli/accounts.go declares no ${name} of raw strings`)
  return Array.from(declared[1].matchAll(/`([^`]*)`/g), part => part[1]).join('')
}

const registered = {
  name: "it's", runtime_token: 'hls_secret', api_url: 'https://api.example/a?ids=1,2', metrics_url: 'https://metrics.example',
  registry: 'registry.example/images', image_tag: 'abc1234', job_image: 'registry.example/images/hypothesisloop-job:abc1234',
}

describe('the scripts of the attach commands', () => {
  it('are the ones hl cluster add prints', () => {
    expect(KUBERNETES_SCRIPT).toBe(goConstant('kubernetesScript'))
    expect(BARE_METAL_SCRIPT).toBe(goConstant('bareMetalScript'))
    expect(BARE_METAL_SCRIPT).toContain('${5:+--cpu-class "$5"}')
  })
})

describe('a start command', () => {
  it('holds no token and quotes what the shell and helm would split', () => {
    for (const kind of ['kubernetes', 'bare-metal'] as const) {
      const command = startCommand(kind, registered, 'tt-host')
      expect(command).not.toContain('hls_secret')
      expect(command).toContain(`it'\\''s'`)
    }
    expect(startCommand('kubernetes', registered)).toContain(`--set-string 'apiURL=https://api.example/a?ids=1\\,2'`)
  })

  it('carries a CPU class as the chart value or the last word, and nothing when there is none', () => {
    const helm = startCommand('kubernetes', registered)
    expect(startCommand('kubernetes', registered, 'tt-host')).toBe(`${helm} --set-string 'cpuClass=tt-host'`)
    expect(helm).not.toContain('cpuClass')
    const node = startCommand('bare-metal', registered)
    expect(startCommand('bare-metal', registered, 'tt-host')).toBe(`${node} 'tt-host'`)
    expect(node.endsWith(`'registry.example/images/hypothesisloop-job:abc1234'`)).toBe(true)
  })
})

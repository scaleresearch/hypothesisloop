import { readFileSync } from 'node:fs'
import { join } from 'node:path'
import { describe, expect, it } from 'vitest'

const ui = join(__dirname, '..')

describe('tsconfig', () => {
  it('covers every build directory the e2e scripts use, so Next never rewrites it', () => {
    const { include } = JSON.parse(readFileSync(join(ui, 'tsconfig.json'), 'utf8')) as { include: string[] }
    for (const script of ['run.mjs', 'walk.mjs']) {
      const dir = /const UI_BUILD = '([^']+)'/.exec(readFileSync(join(ui, 'e2e', script), 'utf8'))?.[1]
      expect(dir, script).toBeTruthy()
      expect(include, script).toContain(`${dir}/types/**/*.ts`)
    }
  })
})

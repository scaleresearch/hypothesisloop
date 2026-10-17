// @vitest-environment node
import { readFileSync } from 'node:fs'
import openapiTS, { astToString, COMMENT_HEADER } from 'openapi-typescript'
import { expect, test } from 'vitest'

test.each([
  ['api.ts', 'openapi.json'],
  ['cluster-agent-api.ts', 'clusters-openapi.json'],
])('src/types/%s is what the published %s gives', async (types, document) => {
  const generated = COMMENT_HEADER + astToString(await openapiTS(new URL(`../../../cmd/control-service/testdata/${document}`, import.meta.url), {
    rootTypes: true, rootTypesNoSchemaPrefix: true, rootTypesKeepCasing: true, enumValues: true, silent: true,
  }))
  const committed = readFileSync(new URL(`./${types}`, import.meta.url), 'utf8')
  expect(generated === committed, `src/types/${types} differs from ${document}: run npm run types`).toBe(true)
})

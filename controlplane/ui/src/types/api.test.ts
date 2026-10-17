// @vitest-environment node
import { readFileSync } from 'node:fs'
import openapiTS, { astToString, COMMENT_HEADER } from 'openapi-typescript'
import { expect, test } from 'vitest'

test('the API types are the ones the published OpenAPI document gives', async () => {
  const document = new URL('../../../cmd/control-service/testdata/openapi.json', import.meta.url)
  const generated = COMMENT_HEADER + astToString(await openapiTS(document, {
    rootTypes: true, rootTypesNoSchemaPrefix: true, rootTypesKeepCasing: true, enumValues: true, silent: true,
  }))
  const committed = readFileSync(new URL('./api.ts', import.meta.url), 'utf8')
  expect(generated === committed, 'src/types/api.ts differs from the OpenAPI document: run make ui-types').toBe(true)
})

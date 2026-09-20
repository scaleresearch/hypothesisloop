// The browser run: the real UI against the running local control plane, signed in through the
// real sign-in code path, with the identity provider stood in for (identity-provider.mjs).
//
//   npm run e2e            from controlplane/ui, with the local stack up (make controlplane-up)
//
// It serves the UI on port 3000, the one origin the local control plane allows, so nothing else
// may be listening there. Every run signs up fresh users, whose orgs stay in the local database
// under names starting with "UI E2E". Screenshots land in e2e/screenshots.

import assert from 'node:assert/strict'
import { execFileSync, spawn } from 'node:child_process'
import { randomUUID } from 'node:crypto'
import { mkdirSync, rmSync } from 'node:fs'
import { dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'
import { chromium } from 'playwright'
import { startIdentityProvider } from './identity-provider.mjs'

const here = dirname(fileURLToPath(import.meta.url))
const repo = join(here, '..', '..', '..')
const shots = join(here, 'screenshots')
const UI = 'http://localhost:3000'
const API = 'http://localhost:8081'
const IDP = 'http://localhost:54399'
const run = randomUUID().slice(0, 8)

// Reduced motion turns the fade-in of panels off, so a screenshot shows them as they end up.
const VIEW = { viewport: { width: 1440, height: 900 }, reducedMotion: 'reduce' }

const person = first => ({ id: randomUUID(), name: `UI E2E ${first} ${run}` })
const ada = person('Ada')
const bob = person('Bob')
const eve = person('Eve')

async function configure(settings) {
  const res = await fetch(`${IDP}/test/configure`, { method: 'POST', body: JSON.stringify(settings) })
  assert.equal(res.status, 200)
}

async function idpState() {
  return (await fetch(`${IDP}/test/state`)).json()
}

async function api(token, path) {
  return fetch(`${API}${path}`, { headers: { Authorization: `Bearer ${token}` } })
}

function hlAdmin(...args) {
  execFileSync('bash', ['-c', '. localdev/lib/auth.sh && lib_hl_admin "$@"', 'hl-admin', ...args], { cwd: repo, stdio: 'inherit' })
}

async function waitForUI() {
  for (let i = 0; i < 120; i++) {
    try {
      if ((await fetch(`${UI}/login`)).ok) return
    } catch {
      // not listening yet
    }
    await new Promise(resolve => setTimeout(resolve, 1000))
  }
  throw new Error('the UI did not come up on port 3000')
}

let step = 0
// `focus` is scrolled into view first: the app scrolls inside its main pane, so a full-page
// screenshot is the viewport.
async function shot(page, name, focus) {
  if (focus) await focus.scrollIntoViewIfNeeded()
  step += 1
  const path = join(shots, `${String(step).padStart(2, '0')}-${name}.png`)
  await page.screenshot({ path, fullPage: true })
  console.log(`  screenshot ${path}`)
}

function section(title) {
  console.log(`\n== ${title}`)
}

// Clicks the one sign-in button and follows the redirects back into the app.
async function signIn(page, user, landing = '**/') {
  await configure({ nextUser: user })
  await page.getByRole('button', { name: 'Continue with GitHub' }).click()
  await page.waitForURL(landing)
}

async function selectInNav(page, label, option) {
  const field = page.locator('.wa-nav-tenant label', { hasText: label }).getByRole('combobox')
  await field.click()
  await page.getByRole('option', { name: option, exact: true }).click()
}

async function main() {
  rmSync(shots, { recursive: true, force: true })
  mkdirSync(shots, { recursive: true })
  const idp = await startIdentityProvider({ port: 54399, keyPath: join(repo, 'localdev', '.dev-jwt-key.pem') })
  const taken = await fetch(UI).then(() => true, () => false)
  assert.ok(!taken, 'port 3000 is already serving something: stop it first')
  const ui = spawn(process.execPath, [join(here, '..', 'node_modules', 'next', 'dist', 'bin', 'next'), 'dev', '-p', '3000'], {
    cwd: join(here, '..'),
    env: {
      ...process.env,
      NEXT_PUBLIC_API_URL: API,
      NEXT_PUBLIC_SUPABASE_URL: IDP,
      NEXT_PUBLIC_SUPABASE_PUBLISHABLE_KEY: 'sb_publishable_browser-run',
    },
    stdio: ['ignore', 'ignore', 'inherit'],
  })
  const browser = await chromium.launch()
  try {
    await waitForUI()
    await scenarios(browser)
    console.log('\nbrowser run passed')
  } catch (error) {
    for (const context of browser.contexts()) {
      for (const page of context.pages()) await shot(page, 'failure')
    }
    throw error
  } finally {
    await browser.close()
    ui.kill()
    idp.close()
  }
}

async function scenarios(browser) {
  const context = await browser.newContext(VIEW)
  await context.grantPermissions(['clipboard-read', 'clipboard-write'], { origin: UI })
  const page = await context.newPage()
  const apiCalls = []
  page.on('request', request => {
    if (request.url().startsWith(API) && request.method() !== 'OPTIONS') apiCalls.push(request)
  })
  page.on('dialog', dialog => dialog.accept())
  const crashes = []
  page.on('pageerror', error => crashes.push(error.message))

  section('a visitor without a session is sent to sign in, and returns to where they were going')
  await page.goto(`${UI}/jobs?status=RUNNING`)
  await page.waitForURL(`${UI}/login?next=%2Fjobs%3Fstatus%3DRUNNING`)
  await page.getByRole('button', { name: 'Continue with GitHub' }).waitFor()
  assert.equal(await page.getByRole('button').count(), 1, 'the login page has one button')
  await shot(page, 'login')

  await signIn(page, ada, `${UI}/jobs?status=RUNNING`)
  const { authorizeRequests } = await idpState()
  const authorize = new URL(authorizeRequests.at(-1))
  assert.equal(authorize.origin + authorize.pathname, `${IDP}/auth/v1/authorize`)
  assert.equal(authorize.searchParams.get('provider'), 'github')
  assert.equal(authorize.searchParams.get('redirect_to'), `${UI}/auth/callback?next=%2Fjobs%3Fstatus%3DRUNNING`)
  assert.equal(authorize.searchParams.get('code_challenge_method'), 's256')
  assert.ok(authorize.searchParams.get('code_challenge'))

  section('the session is in cookies, never in local storage or a URL, and every API call carries it')
  await page.getByText('Sign out').waitFor()
  assert.deepEqual(await page.evaluate(() => Object.keys(localStorage)), [])
  assert.ok((await context.cookies()).some(c => c.name.includes('auth-token')), 'the session cookie is set')
  assert.ok(apiCalls.length > 0)
  for (const call of apiCalls) {
    assert.match(call.headers().authorization ?? '', /^Bearer ey/, `${call.url()} carries the token`)
    assert.ok(!call.url().includes('ey'), 'no token in a URL')
  }
  const listCalls = apiCalls.filter(c => /\/(experiments|platform-experiments|agents|donations)(\/stats)?(\?|$)/.test(new URL(c.url()).pathname + '?'))
  for (const call of listCalls) {
    assert.ok(new URL(call.url()).searchParams.get('project_id'), `${call.url()} names the project`)
  }

  section('a first-time user lands in their own org and default project, with what to do next')
  await page.goto(UI)
  await page.getByText('is empty. Start here.').waitFor()
  await page.locator('.wa-nav-tenant').getByRole('combobox').first().waitFor()
  assert.equal(await page.locator('.wa-nav-tenant').getByRole('combobox').first().inputValue(), ada.name)
  assert.equal(await page.locator('.wa-nav-tenant').getByRole('combobox').nth(1).inputValue(), 'default')
  await shot(page, 'first-run-home')
  await page.getByRole('link', { name: 'Create a platform experiment' }).click()
  await page.getByText('has no platform experiments yet').waitFor()
  await shot(page, 'first-run-empty-project')

  section('creating a platform experiment in the selected project')
  await page.getByRole('button', { name: 'Create the first platform experiment' }).click()
  const experimentName = `ui-e2e-${run}`
  await page.getByPlaceholder('e.g. Q3 Vision Benchmark').fill(experimentName)
  await page.locator('select', { hasText: 'pick a common metric' }).selectOption({ index: 1 })
  await page.getByRole('button', { name: 'Add', exact: true }).click()
  await shot(page, 'create-platform-experiment')
  await page.getByRole('button', { name: 'Next: competition settings' }).click()
  const created = page.waitForRequest(r => r.url() === `${API}/platform-experiments` && r.method() === 'POST')
  await page.getByRole('button', { name: 'Create Experiment' }).click()
  const createBody = (await created).postDataJSON()
  const adaMe = await (await api((await created).headers().authorization.slice(7), '/me')).json()
  assert.equal(createBody.project_id, adaMe.projects[0].id, 'the new experiment is sent with the project')
  await page.getByText(experimentName).first().waitFor()
  await shot(page, 'platform-experiment-created')

  section('adding and removing agents on the experiment page')
  await page.getByText(experimentName).first().click()
  await page.waitForURL(/\/platform-experiments\/[^/?]+/)
  const experimentURL = page.url()
  await page.getByText('No agents yet.').waitFor()
  await page.getByText('Count').locator('..').getByRole('textbox').fill('2')
  await page.getByPlaceholder("agent's default").first().fill('hyperparameter-search')
  await page.getByRole('button', { name: 'Me', exact: true }).click()
  await page.getByRole('button', { name: 'Add 2 agents' }).click()
  await page.getByTestId('secret').nth(1).waitFor()
  const agentEnv = await page.getByTestId('secret').first().innerText()
  assert.match(agentEnv, /^AGENT_ID=ag-/)
  assert.match(agentEnv, /HL_TOKEN=hls_/)
  assert.match(agentEnv, /AGENT_FLAVOR=hyperparameter-search/)
  await shot(page, 'self-run-agent-tokens', page.getByTestId('secret').first())
  const agentToken = agentEnv.match(/HL_TOKEN=(\S+)/)[1]
  assert.equal((await api(agentToken, `/platform-experiments/${experimentURL.split('/').pop().split('?')[0]}`)).status, 200, 'the agent token works')
  await page.reload()
  await page.getByRole('row').nth(2).waitFor()
  assert.equal(await page.getByTestId('secret').count(), 0, 'a reload does not bring an agent token back')
  assert.ok(!(await page.content()).includes('hls_'), 'no token anywhere in the page')
  await page.getByRole('button', { name: 'The platform' }).click()
  await page.getByText('Count').locator('..').getByRole('textbox').fill('1')
  await page.getByRole('button', { name: 'Add agent' }).click()
  await page.getByRole('cell', { name: 'the platform', exact: true }).waitFor()
  assert.equal(await page.getByTestId('secret').count(), 0, 'a hosted agent has no token to show')
  const agentRows = page.locator('table', { hasText: 'Run by' }).locator('tbody tr')
  assert.equal(await agentRows.count(), 3)
  await agentRows.first().getByRole('button', { name: 'Remove' }).click()
  await page.waitForFunction(() => document.querySelectorAll('table').length > 0 && [...document.querySelectorAll('table')].some(t => t.textContent.includes('Run by') && t.querySelectorAll('tbody tr').length === 2))
  assert.equal((await api(agentToken, '/me')).status, 401, 'a removed agent\'s token stops working')
  await shot(page, 'experiment-agents', agentRows.last())

  section('the idea form has no name field and the idea is filed under the signed-in name')
  await page.goto(`${UI}/hypotheses?pe=${experimentURL.split('/').pop().split('?')[0]}`)
  await page.getByRole('button', { name: '+ Add idea under the objective' }).click()
  await page.getByText('registered under the name you signed in with').waitFor()
  assert.equal(await page.getByPlaceholder(/your name/i).count(), 0)

  section('settings: a second project, then switching projects shows only what each holds')
  await page.goto(`${UI}/settings`)
  await page.getByLabel('New project name').fill('second')
  await page.getByRole('button', { name: 'Create project' }).click()
  await page.getByLabel('Name of project second').waitFor()
  await selectInNav(page, 'Project', 'second')
  await page.waitForURL(/\/settings\?project=/)
  await page.getByRole('link', { name: 'Platform Experiments' }).click()
  await page.waitForURL(/\/platform-experiments\?project=/)
  await page.getByText('second has no platform experiments yet').waitFor()
  assert.equal(await page.getByText(experimentName).count(), 0, 'the other project\'s experiment is not listed')
  await shot(page, 'switched-project-is-empty')
  await selectInNav(page, 'Project', 'default')
  await page.getByText(experimentName).first().waitFor()
  await shot(page, 'switched-back-shows-experiment')

  section('settings: renaming the project and the org')
  await page.goto(`${UI}/settings`)
  await page.getByLabel('Name of project second').fill('renamed')
  await page.getByLabel('Name of project second').locator('..').getByRole('button', { name: 'Rename' }).click()
  await page.getByLabel('Name of project renamed').waitFor()
  await page.getByLabel('New project name').fill('default')
  await page.getByRole('button', { name: 'Create project' }).click()
  await page.getByText('already').waitFor()
  await page.getByLabel('Organization name').fill(`${ada.name} Lab`)
  await page.getByLabel('Organization name').locator('..').getByRole('button', { name: 'Rename' }).click()
  await page.waitForFunction(name => document.querySelector('.wa-nav-tenant input')?.value === name, `${ada.name} Lab`)

  section('settings: an API key is shown once, works, and stops working when revoked')
  await page.getByLabel('Key name').fill('laptop')
  await page.getByRole('button', { name: 'Create API key' }).click()
  const keyToken = await page.getByTestId('secret').innerText()
  assert.match(keyToken, /^hlk_/)
  await shot(page, 'api-key-created', page.getByTestId('secret'))
  await page.getByRole('button', { name: 'Copy' }).click()
  assert.equal(await page.evaluate(() => navigator.clipboard.readText()), keyToken)
  assert.equal((await (await api(keyToken, '/me')).json()).kind, 'key')
  await page.getByRole('button', { name: 'I have saved it' }).click()
  assert.equal(await page.getByTestId('secret').count(), 0)
  await page.reload()
  await page.getByRole('cell', { name: 'laptop' }).waitFor()
  assert.ok(!(await page.content()).includes(keyToken), 'the token is not on the page after a reload')
  await page.getByRole('button', { name: 'Revoke' }).click()
  await page.getByText(/^revoked /).waitFor()
  assert.equal((await api(keyToken, '/me')).status, 401, 'a revoked key is refused')
  await shot(page, 'api-key-revoked', page.getByText(/^revoked /))

  section('settings: adding a cluster shows its start command once, and the cluster page lists it')
  const clusterName = `ui-e2e-${run}`
  await page.getByLabel('Cluster name').fill(clusterName)
  await page.getByRole('button', { name: 'Add cluster' }).click()
  const command = await page.getByTestId('secret').innerText()
  assert.match(command, new RegExp(`^make cluster-agent-up CLUSTER=${clusterName} API_URL=${API} METRICS_URL=<metrics-service-url> RUNTIME_TOKEN=hls_`))
  await page.getByRole('cell', { name: clusterName }).waitFor()
  await shot(page, 'cluster-added', page.getByRole('cell', { name: clusterName }))
  await page.getByText('Repository').first().waitFor()
  await page.getByText('Not available yet.').waitFor()
  await page.getByRole('link', { name: 'Compute Resources' }).click()
  await page.getByRole('cell', { name: clusterName }).waitFor()
  await page.getByText('Disconnected').first().waitFor()
  assert.equal(await page.getByText('staff only').count(), 0, 'no staff view for a customer')
  await shot(page, 'cluster-page')
  await page.getByRole('link', { name: 'Dashboard' }).click()
  await page.getByText('Job Status Breakdown').waitFor()
  assert.equal(await page.getByText('staff only').count(), 0)

  section('a person in two orgs switches between them, and each shows its own work')
  const adaToken = (await (await fetch(`${IDP}/test/token`, { method: 'POST', body: JSON.stringify({ user: ada }) })).json()).access_token
  const secondOrg = await fetch(`${API}/orgs`, {
    method: 'POST',
    headers: { Authorization: `Bearer ${adaToken}`, 'Content-Type': 'application/json' },
    body: JSON.stringify({ name: `${ada.name} Second` }),
  })
  assert.equal(secondOrg.status, 201)
  await page.goto(`${UI}/platform-experiments`)
  await page.getByText(experimentName).first().waitFor()
  await selectInNav(page, 'Organization', `${ada.name} Second`)
  await page.getByText(`${ada.name} Second / default has no platform experiments yet`).waitFor()
  assert.equal(await page.getByText(experimentName).count(), 0)
  await shot(page, 'second-org-is-empty')
  await page.getByRole('link', { name: 'Settings' }).click()
  await page.getByRole('heading', { name: 'Settings' }).waitFor()
  assert.equal(await page.getByRole('cell', { name: 'laptop' }).count(), 0, 'the other org\'s API key is not listed')
  assert.equal(await page.getByRole('cell', { name: clusterName }).count(), 0, 'the other org\'s cluster is not listed')
  await selectInNav(page, 'Organization', `${ada.name} Lab`)
  await page.getByRole('cell', { name: clusterName }).waitFor()

  section('an API that fails is said to fail: no empty page')
  await page.route(`${API}/me`, route => route.abort())
  await page.goto(`${UI}/jobs`)
  await page.getByText('Cannot reach the API to load your account').waitFor()
  assert.equal(await page.getByRole('heading', { name: 'Jobs' }).count(), 0)
  await shot(page, 'api-unreachable')
  await page.unroute(`${API}/me`)
  await page.getByRole('heading', { name: 'Jobs' }).waitFor({ timeout: 60_000 })

  section('editing the experiment keeps it in its project')
  await page.goto(`${UI}/platform-experiments`)
  await page.getByRole('button', { name: 'Edit' }).first().click()
  await page.getByPlaceholder('e.g. Q3 Vision Benchmark').fill(`${experimentName}-edited`)
  await page.getByRole('button', { name: 'Next: competition settings' }).click()
  await page.getByRole('button', { name: 'Save Changes' }).click()
  await page.getByText(`${experimentName}-edited`).first().waitFor()

  section('a second account sees none of the first one\'s work')
  const other = await browser.newContext(VIEW)
  const otherPage = await other.newPage()
  await otherPage.goto(`${UI}/platform-experiments`)
  await signIn(otherPage, bob, `${UI}/platform-experiments`)
  await otherPage.getByText('has no platform experiments yet').waitFor()
  assert.equal(await otherPage.getByText(experimentName).count(), 0)
  await shot(otherPage, 'second-account-sees-nothing')
  await otherPage.goto(experimentURL.split('?')[0])
  await otherPage.getByText(/not found|could not|failed/i).first().waitFor()
  await shot(otherPage, 'second-account-opens-foreign-experiment')
  await otherPage.goto(`${UI}/jobs?project=${adaMe.projects[0].id}`)
  await otherPage.getByText('is not available to you').waitFor()
  await shot(otherPage, 'foreign-project-in-url')
  await otherPage.getByRole('button', { name: /^Go to / }).click()
  await otherPage.getByRole('heading', { name: 'Jobs' }).waitFor()

  section('staff see the views across every org, and a customer does not')
  hlAdmin('user', 'staff', bob.id, 'on')
  try {
    await otherPage.goto(`${UI}/cluster`)
    await otherPage.getByText(`Clusters of ${bob.name}`).waitFor()
    await otherPage.getByText('All Clusters · every org · staff only').waitFor()
    await otherPage.getByRole('columnheader', { name: 'Autoscaler' }).waitFor()
    await shot(otherPage, 'staff-cluster-page', otherPage.getByRole('columnheader', { name: 'Autoscaler' }))
    await otherPage.goto(`${UI}/dashboard`)
    await otherPage.getByText('Scheduler Quality · every org · staff only').waitFor()
    await otherPage.getByText('Burst eviction rate').waitFor()
    await shot(otherPage, 'staff-dashboard', otherPage.getByText('Burst eviction rate'))
  } finally {
    hlAdmin('user', 'staff', bob.id, 'off')
  }

  section('sign-out ends the session')
  await otherPage.getByRole('button', { name: 'Sign out' }).click()
  await otherPage.waitForURL(`${UI}/login`)
  await otherPage.goto(`${UI}/jobs`)
  await otherPage.waitForURL(`${UI}/login?next=%2Fjobs`)
  await other.close()

  section('an expiring token is refreshed and requests go on with the new one')
  const refreshesBefore = (await idpState()).refreshes
  await configure({ tokenLifetimeSeconds: 8 })
  const shortLived = await browser.newContext(VIEW)
  const shortPage = await shortLived.newPage()
  const tokens = new Set()
  const statuses = []
  shortPage.on('request', r => { if (r.url().startsWith(`${API}/me`) && r.method() === 'GET') tokens.add(r.headers().authorization) })
  shortPage.on('response', r => { if (r.url().startsWith(API)) statuses.push(r.status()) })
  await shortPage.goto(`${UI}/jobs`)
  await signIn(shortPage, eve, `${UI}/jobs`)
  await shortPage.getByRole('heading', { name: 'Jobs' }).waitFor()
  await shortPage.waitForTimeout(10_000)
  await shortPage.goto(`${UI}/agents`)
  await shortPage.getByRole('heading', { name: 'Research Agents' }).waitFor()
  await shortPage.waitForTimeout(1_000)
  assert.ok((await idpState()).refreshes > refreshesBefore, 'the session was refreshed')
  assert.ok(tokens.size > 1, 'requests after the refresh carry a new token')
  assert.ok(!statuses.includes(401), 'no request was refused across the refresh')

  section('a session that can no longer be refreshed ends at the sign-in page, with the way back')
  await configure({ revoke: eve.id })
  await shortPage.waitForURL(/\/login\?notice=ended&next=/, { timeout: 60_000 })
  await shortPage.getByText('Your session ended.').waitFor()
  await shot(shortPage, 'session-ended')
  await shortLived.close()

  section('a token the API refuses ends at the sign-in page and does not loop')
  await configure({ tokenLifetimeSeconds: 3600, audience: 'some-other-api' })
  const refused = await browser.newContext(VIEW)
  const refusedPage = await refused.newPage()
  await refusedPage.goto(`${UI}/agents`)
  await signIn(refusedPage, eve, /\/login\?notice=refused&next=%2Fagents/)
  await refusedPage.getByText('The API did not accept your session.').waitFor()
  await refusedPage.waitForTimeout(1_500)
  assert.match(refusedPage.url(), /\/login\?notice=refused/, 'the page stays on sign-in')
  await shot(refusedPage, 'session-refused')
  await refused.close()
  await configure({ audience: 'hypothesisloop-dev' })

  assert.deepEqual(crashes, [], 'no page threw an uncaught error')
  await context.close()
}

main().catch(error => {
  console.error(error)
  process.exit(1)
})

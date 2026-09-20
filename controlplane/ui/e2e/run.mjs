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
const IDP = 'http://127.0.0.1:54399'
// No API and no hl-admin command adds a person to an org yet, so the member scenario writes the
// membership row itself, in the local stack's database.
const POSTGRES_CONTAINER = 'controlplane-postgres-1'
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

// A token of the user straight from the identity provider, for reading back what a page did.
async function tokenOf(user) {
  const res = await fetch(`${IDP}/test/token`, { method: 'POST', body: JSON.stringify({ user }) })
  return (await res.json()).access_token
}

async function api(token, path) {
  return fetch(`${API}${path}`, { headers: { Authorization: `Bearer ${token}` } })
}

function sql(statement) {
  execFileSync('podman', ['exec', '-i', POSTGRES_CONTAINER, 'psql', '-U', 'hypothesisloop', '-d', 'hypothesisloop', '-v', 'ON_ERROR_STOP=1', '-q'], { input: statement, stdio: ['pipe', 'inherit', 'inherit'] })
}

const crashes = []
const requested = []
// Everything that must never appear in a URL: each bearer token a page sent and each secret a page showed.
const secrets = new Set()

// Every context reports its uncaught page errors and every URL it asked for.
async function newContext(browser) {
  const context = await browser.newContext(VIEW)
  context.on('weberror', webError => crashes.push(`${webError.page()?.url()}: ${webError.error().message}`))
  context.on('request', request => {
    requested.push(request.url())
    const authorization = request.headers().authorization
    if (authorization?.startsWith('Bearer ')) secrets.add(authorization.slice(7))
  })
  return context
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

// Opens the "New ..." field in the switcher, types a name and submits it.
async function createInNav(page, what, name) {
  await page.getByRole('button', { name: `+ New ${what}` }).click()
  await page.getByLabel(`New ${what} name`).first().fill(name)
  await page.locator('.wa-nav-create').getByRole('button', { name: 'Create' }).click()
}

// What a cluster agent with nothing to offer reports: its identity and no capacity. One report
// is what binds the cluster's settings to it.
function idleReport(clusterID) {
  return {
    cluster_id: clusterID,
    cpu_available_cores: 0, cpu_total_cores: 0,
    ram_available_bytes: 0, ram_total_bytes: 0,
    storage_available_bytes: 0, storage_total_bytes: 0,
    accelerator_available_by_type: {}, accelerator_total_by_type: {},
    accelerator_available_by_node: {}, node_resources_by_node: {},
    node_resources_total_by_node: {}, node_labels_by_node: {},
    multi_node_capable: false, autoscaler_enabled: false,
  }
}

async function main() {
  rmSync(shots, { recursive: true, force: true })
  mkdirSync(shots, { recursive: true })
  const idp = await startIdentityProvider({ port: 54399, keyPath: join(repo, 'localdev', '.dev-jwt-key.pem'), ui: UI })
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
  section('a sign-in that does not complete says so')
  const visitor = await newContext(browser)
  const visitorPage = await visitor.newPage()
  await visitorPage.goto(`${UI}/auth/callback?code=not-a-code&next=%2Fjobs`)
  await visitorPage.waitForURL(`${UI}/login?notice=failed`)
  await visitorPage.getByText('Sign-in did not complete. Try again.').waitFor()
  await shot(visitorPage, 'sign-in-failed')
  await visitorPage.goto(`${UI}/login?notice=__proto__`)
  await visitorPage.getByRole('button', { name: 'Continue with GitHub' }).waitFor()
  assert.equal(await visitorPage.locator('.error-banner').count(), 0, 'an unknown notice shows nothing')

  section('a way back that leads off the site is dropped, signed out and signed in')
  const HOSTILE = ['/%09/evil.example', '/%0A/evil.example', '/%5Cevil.example', '%2F%2Fevil.example', 'https%3A%2F%2Fevil.example']
  await visitorPage.goto(`${UI}/login?next=${HOSTILE[0]}`)
  await signIn(visitorPage, eve, `${UI}/`)
  assert.equal(new URL((await idpState()).authorizeRequests.at(-1)).searchParams.get('redirect_to'), `${UI}/auth/callback?next=%2F`)
  for (const next of HOSTILE) {
    await visitorPage.goto(`${UI}/login?next=${next}`)
    await visitorPage.waitForURL(`${UI}/`)
  }
  await visitor.close()

  const context = await newContext(browser)
  await context.grantPermissions(['clipboard-read', 'clipboard-write'], { origin: UI })
  const page = await context.newPage()
  const apiCalls = []
  page.on('request', request => {
    if (request.url().startsWith(API) && request.method() !== 'OPTIONS') apiCalls.push(request)
  })
  page.on('dialog', dialog => dialog.accept())

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
  const agentID = agentEnv.match(/AGENT_ID=(\S+)/)[1]
  const experimentID = new URL(experimentURL).pathname.split('/').pop()
  secrets.add(agentToken)
  assert.equal((await api(agentToken, `/platform-experiments/${experimentID}`)).status, 200, 'the agent token works')
  await page.reload()
  await page.getByRole('row').nth(2).waitFor()
  assert.equal(await page.getByTestId('secret').count(), 0, 'a reload takes the tokens off the screen')
  assert.ok(!(await page.content()).includes('hls_'), 'no token anywhere in the page')
  await page.getByRole('button', { name: 'Show token' }).first().click()
  assert.equal(await page.getByTestId('secret').innerText(), agentEnv, 'asking again shows the same token')
  await page.getByText('brings the same one back').waitFor()
  await page.getByRole('button', { name: 'Hide' }).click()
  assert.equal(await page.getByTestId('secret').count(), 0)
  await page.getByRole('button', { name: 'The platform' }).click()
  await page.getByText('Count').locator('..').getByRole('textbox').fill('1')
  await page.getByRole('button', { name: 'Add agent' }).click()
  await page.getByRole('cell', { name: 'the platform', exact: true }).waitFor()
  assert.equal(await page.getByTestId('secret').count(), 0, 'a hosted agent has no token to show')
  const agentRows = page.locator('table', { hasText: 'Run by' }).locator('tbody tr')
  assert.equal(await agentRows.count(), 3)
  assert.equal((await api(agentToken, `/platform-experiments/${experimentID}`)).status, 200, 'the agent token still works before its agent is removed')
  await agentRows.filter({ hasText: agentID }).getByRole('button', { name: 'Remove' }).click()
  await agentRows.filter({ hasText: agentID }).waitFor({ state: 'detached' })
  assert.equal(await agentRows.count(), 2)
  assert.equal((await api(agentToken, `/platform-experiments/${experimentID}`)).status, 401, 'the same request is refused once the agent is removed')
  await shot(page, 'experiment-agents', agentRows.last())

  section('the idea form has no name field and the idea is filed under the signed-in name')
  await page.goto(`${UI}/hypotheses?pe=${experimentID}`)
  await page.getByRole('button', { name: '+ Add idea under the objective' }).click()
  await page.getByText('registered under the name you signed in with').waitFor()
  assert.equal(await page.getByPlaceholder(/your name/i).count(), 0)

  section('settings: a second project, then switching projects shows only what each holds')
  await page.goto(`${UI}/settings`)
  await createInNav(page, 'project', 'default')
  await page.locator('.wa-nav-create').getByText('already').waitFor()
  await page.getByLabel('New project name').first().fill('second')
  await page.locator('.wa-nav-create').getByRole('button', { name: 'Create' }).click()
  await page.waitForURL(/\/settings\?project=/)
  assert.equal(await page.locator('.wa-nav-tenant').getByRole('combobox').nth(1).inputValue(), 'second', 'the new project is selected')
  await page.getByLabel('Name of project second').waitFor()
  const secondProjectID = new URL(page.url()).searchParams.get('project')

  section('a record opened under another project puts its own project in the URL')
  await page.goto(`${UI}/platform-experiments/${experimentID}?project=${secondProjectID}`)
  await page.waitForURL(`${UI}/platform-experiments/${experimentID}?project=${adaMe.projects[0].id}`)
  await page.getByText(experimentName).first().waitFor()
  assert.equal(await page.locator('.wa-nav-tenant').getByRole('combobox').nth(1).inputValue(), 'default', 'the switcher names the record\'s project')
  await page.goto(`${UI}/settings?project=${secondProjectID}`)
  await page.getByLabel('Name of project second').waitFor()
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
  await page.getByLabel('Name of project second').fill('default')
  await page.getByLabel('Name of project second').locator('..').getByRole('button', { name: 'Rename' }).click()
  await page.getByLabel('Name of project second').locator('..').getByText('already').waitFor()
  await shot(page, 'project-rename-collision', page.getByLabel('Name of project second'))
  await page.getByLabel('Name of project second').fill('renamed')
  await page.getByLabel('Name of project second').locator('..').getByRole('button', { name: 'Rename' }).click()
  await page.getByLabel('Name of project renamed').waitFor()
  await page.getByLabel('New project name').last().fill('default')
  await page.getByRole('button', { name: 'Create project' }).click()
  await page.getByLabel('New project name').last().locator('..').getByText('already').waitFor()
  await page.getByLabel('Organization name').fill(`${ada.name} Lab`)
  await page.getByLabel('Organization name').locator('..').getByRole('button', { name: 'Rename' }).click()
  await page.waitForFunction(name => document.querySelector('.wa-nav-tenant input')?.value === name, `${ada.name} Lab`)

  section('settings: an API key is shown once, works, and stops working when revoked')
  await page.getByLabel('Key name').fill('laptop')
  await page.getByRole('button', { name: 'Create API key' }).dblclick()
  const keyToken = await page.getByTestId('secret').innerText()
  assert.match(keyToken, /^hlk_/)
  secrets.add(keyToken)
  await shot(page, 'api-key-created', page.getByTestId('secret'))
  await page.getByRole('button', { name: 'Copy' }).click()
  assert.equal(await page.evaluate(() => navigator.clipboard.readText()), keyToken)
  assert.equal((await (await api(keyToken, '/me')).json()).kind, 'key')
  await page.getByRole('button', { name: 'I have saved it' }).click()
  assert.equal(await page.getByTestId('secret').count(), 0)
  await page.reload()
  await page.getByRole('cell', { name: 'laptop' }).first().waitFor()
  assert.equal(await page.getByRole('cell', { name: 'laptop' }).count(), 1, 'a double click created one key')
  assert.ok(!(await page.content()).includes(keyToken), 'the token is not on the page after a reload')
  await page.getByRole('button', { name: 'Revoke' }).click()
  await page.getByText(/^revoked /).waitFor()
  assert.equal((await api(keyToken, '/me')).status, 401, 'a revoked key is refused')
  await shot(page, 'api-key-revoked', page.getByText(/^revoked /))

  section('settings: adding a cluster shows its start command, which can be shown again')
  const clusterName = `ui-e2e-${run}`
  await page.getByLabel('Cluster name').fill(clusterName)
  await page.getByRole('button', { name: 'Add cluster' }).click()
  const command = await page.getByTestId('secret').innerText()
  assert.match(
    command,
    new RegExp(`^RUNTIME_TOKEN="\\$\\(hl cluster token '${clusterName}' --org '${adaMe.orgs[0].id}'\\)" make cluster-agent-up CLUSTER='${clusterName}' API_URL='https?://[^']+' METRICS_URL='https?://[^']+'$`),
    'the command is the one hl cluster add prints, with the addresses the platform answered with',
  )
  const registered = await (await fetch(`${API}/orgs/${adaMe.orgs[0].id}/clusters`, {
    method: 'POST',
    headers: { Authorization: `Bearer ${await tokenOf(ada)}`, 'Content-Type': 'application/json' },
    body: JSON.stringify({ name: clusterName }),
  })).json()
  const runtimeToken = registered.runtime_token
  assert.match(runtimeToken, /^hls_/)
  secrets.add(runtimeToken)
  assert.ok(!(await page.content()).includes(runtimeToken), 'the runtime token is nowhere on the page')
  assert.ok(command.includes(`API_URL='${registered.api_url}' METRICS_URL='${registered.metrics_url}'`))
  await page.getByRole('cell', { name: clusterName }).waitFor()
  await shot(page, 'cluster-added', page.getByRole('cell', { name: clusterName }))
  await page.getByRole('button', { name: 'Hide' }).click()
  await page.getByRole('button', { name: 'Show start command' }).click()
  assert.equal(await page.getByTestId('secret').innerText(), command, 'asking again shows the same command')
  assert.ok(!(await page.content()).includes(runtimeToken))
  await page.getByRole('button', { name: 'Hide' }).click()

  section('the settings editor on an owned cluster, once its agent has reported')
  const clusterID = `id-${clusterName}`
  const reported = await fetch(`${API}/internal/clusters/${clusterName}/reconcile`, {
    method: 'POST',
    headers: { Authorization: `Bearer ${runtimeToken}`, 'Content-Type': 'application/json' },
    body: JSON.stringify(idleReport(clusterID)),
  })
  assert.equal(reported.status, 200, `the stand-in agent's report: ${await reported.clone().text()}`)
  const clusterRow = page.getByRole('row', { name: new RegExp(clusterName) })
  await clusterRow.getByText('Connected', { exact: true }).waitFor()
  await clusterRow.getByText('stuck-pending timeout: default').waitFor()
  await clusterRow.getByRole('button', { name: 'Edit' }).click()
  await clusterRow.getByLabel('stuck-pending timeout').fill('900')
  await clusterRow.getByLabel('scale-up timeout').fill('5000')
  await clusterRow.getByRole('button', { name: 'Save' }).click()
  await clusterRow.locator('.text-error').waitFor()
  await shot(page, 'cluster-settings-refused', clusterRow)
  await clusterRow.getByLabel('scale-up timeout').fill('600')
  await clusterRow.getByRole('button', { name: 'Save' }).click()
  await clusterRow.getByText('scale-up timeout: 600s · speculative cap: default · stuck-pending timeout: 900s').waitFor()
  await shot(page, 'cluster-settings-saved', clusterRow)
  const savedSettings = await (await api(await tokenOf(ada), `/clusters/${clusterID}/settings`)).json()
  assert.equal(savedSettings.stuck_pending_timeout_seconds, 900)
  assert.equal(savedSettings.scale_up_timeout_seconds, 600)
  assert.equal(savedSettings.max_speculative_accelerators ?? null, null)
  const capacityRead = page.waitForRequest(r => r.url().startsWith(`${API}/resource-catalog/capacity`))
  await page.getByRole('link', { name: 'Compute Resources' }).click()
  await page.getByRole('heading', { name: 'Compute Resources' }).waitFor()
  assert.equal(new URL((await capacityRead).url()).searchParams.get('org_id'), adaMe.orgs[0].id, 'capacity is read for the selected org')
  await page.getByRole('cell', { name: new RegExp(clusterName) }).waitFor()
  assert.equal(await page.getByRole('button', { name: 'Show start command' }).count(), 0, 'the cluster page manages nothing')
  assert.equal(await page.getByText('staff only').count(), 0, 'no staff view for a customer')
  await shot(page, 'cluster-page')
  await page.getByRole('link', { name: 'Dashboard' }).click()
  await page.getByText('Job Status Breakdown').waitFor()
  assert.equal(await page.getByText('staff only').count(), 0)

  section('repository: a deployment without a GitHub App says so')
  const repoPage = `${UI}/settings/repository?project=${secondProjectID}`
  const repoAPI = `${API}/projects/${secondProjectID}/repo`
  const appRead = page.waitForResponse(`${API}/github/app`)
  const repoRead = page.waitForResponse(r => r.url() === repoAPI && r.request().method() === 'GET')
  await page.goto(`${UI}/settings?project=${secondProjectID}`)
  await page.getByRole('link', { name: 'Repository of renamed' }).click()
  assert.equal((await repoRead).status(), 404, 'the project has no repository')
  assert.equal((await appRead).status(), 503, 'the local stack has no GitHub App')
  await page.getByText('GitHub is not configured on this deployment').waitFor()
  assert.equal(await page.getByRole('button', { name: 'Install the GitHub App' }).count(), 0)
  await shot(page, 'repository-github-not-configured')

  section('repository: install, authorise and name the repository, with GitHub stood in for')
  const cors = { 'access-control-allow-origin': UI }
  await page.route(`${API}/github/app`, route => route.fulfill({
    headers: cors,
    json: { slug: 'hl-e2e', client_id: 'Iv1.e2e', install_url: `${IDP}/github/install`, authorize_url: `${IDP}/github/authorize?client_id=Iv1.e2e` },
  }))
  await page.goto(repoPage)
  await page.getByText('No repository is attached to renamed.').waitFor()
  await shot(page, 'repository-none-attached')
  const leaves = []
  const watchLeaves = request => { if (request.url().startsWith(`${IDP}/github/`)) leaves.push(new URL(request.url())) }
  page.on('request', watchLeaves)
  await page.getByRole('button', { name: 'Install the GitHub App' }).click()
  await page.getByLabel('Repository').waitFor()
  page.off('request', watchLeaves)
  assert.deepEqual(leaves.map(u => u.pathname), ['/github/install', '/github/authorize'], 'the person went to install, then straight on to authorise')
  const [installed, authorized] = leaves
  assert.match(installed.searchParams.get('state'), /^[0-9a-f-]{36}$/)
  assert.equal(authorized.searchParams.get('state'), `${installed.searchParams.get('state')}.4242`, 'the state carries the installation')
  assert.equal(authorized.searchParams.get('redirect_uri'), `${UI}/settings/repository`, 'the way back is this UI\'s own origin')
  assert.equal(authorized.searchParams.get('client_id'), 'Iv1.e2e')
  const githubCode = (await idpState()).githubCodes.at(-1)
  const returned = new URL(page.url())
  assert.equal(returned.searchParams.get('code'), githubCode)
  assert.equal(returned.searchParams.get('project'), secondProjectID, 'the return is back in the project the attach began in')
  assert.equal(await page.locator('.wa-nav-tenant').getByRole('combobox').nth(1).inputValue(), 'renamed')
  await shot(page, 'repository-name-it')

  // The real API takes the request and refuses it, having no App: its words are shown as they are.
  const refusedAttach = page.waitForResponse(r => r.url() === repoAPI && r.request().method() === 'POST')
  await page.getByLabel('Repository').fill('acme/widgets')
  await page.getByRole('button', { name: 'Attach repository' }).click()
  const attachAnswer = await refusedAttach
  assert.deepEqual(attachAnswer.request().postDataJSON(), { installation_id: 4242, repo: 'acme/widgets', code: githubCode, redirect_uri: `${UI}/settings/repository` })
  assert.equal(attachAnswer.status(), 503)
  const attachRefusal = await attachAnswer.json()
  await page.locator('.error-banner').waitFor()
  assert.equal((await page.locator('.error-banner').innerText()).trim(), attachRefusal.message ?? attachRefusal.error)
  assert.equal(await page.getByLabel('Repository').inputValue(), 'acme/widgets')
  await shot(page, 'repository-attach-refused')

  // With the API's repository calls stood in for as well, the same form attaches, shows and detaches.
  let attachedRepo = null
  const repoCalls = []
  await page.route(repoAPI, route => {
    const method = route.request().method()
    if (method === 'OPTIONS') return route.fallback()
    repoCalls.push(method)
    if (method === 'POST') {
      attachedRepo = {
        project_id: secondProjectID, installation_id: 4242, repository_id: 9, repo: route.request().postDataJSON().repo,
        attached_at: new Date().toISOString(), clone_url: 'https://github.com/acme/widgets.git',
      }
      return route.fulfill({ headers: cors, json: attachedRepo })
    }
    if (method === 'DELETE') {
      attachedRepo = null
      return route.fulfill({ status: 204, headers: cors })
    }
    return attachedRepo
      ? route.fulfill({ headers: cors, json: attachedRepo })
      : route.fulfill({ status: 404, headers: cors, json: { message: 'no repository is attached to this project' } })
  })
  await page.getByRole('button', { name: 'Attach repository' }).dblclick()
  await page.getByRole('button', { name: 'Detach repository' }).waitFor()
  assert.equal(repoCalls.filter(m => m === 'POST').length, 1, 'a double click attached once')
  assert.equal(page.url(), repoPage, 'the code and state are out of the URL')
  assert.equal(await page.evaluate(() => sessionStorage.length), 0, 'the attempt is over')
  await page.getByRole('cell', { name: 'acme/widgets', exact: true }).waitFor()
  await page.getByRole('cell', { name: 'https://github.com/acme/widgets.git' }).waitFor()
  await shot(page, 'repository-attached')
  await page.getByRole('button', { name: 'Detach repository' }).click()
  await page.getByText('No repository is attached to renamed.').waitFor()
  assert.equal(repoCalls.filter(m => m === 'DELETE').length, 1)
  await page.unroute(repoAPI)

  section('repository: a return from GitHub this tab did not start is refused')
  await page.goto(`${repoPage}&code=stolen&state=forged.4242`)
  await page.getByText('does not belong to an attach started in this tab').waitFor()
  assert.equal(await page.getByLabel('Repository').count(), 0)
  await shot(page, 'repository-return-refused')
  await page.getByRole('button', { name: 'Install the GitHub App' }).waitFor({ state: 'detached' })
  await page.getByRole('link', { name: 'Start again' }).click()
  await page.getByRole('button', { name: 'Install the GitHub App' }).click()
  await page.getByLabel('Repository').waitFor()
  const midway = new URL(page.url())
  midway.searchParams.set('state', `11111111-1111-1111-1111-111111111111.${4242}`)
  await page.goto(midway.toString())
  await page.getByText('does not belong to an attach started in this tab').waitFor()
  await page.unroute(`${API}/github/app`)
  await page.evaluate(() => sessionStorage.clear())

  section('a person in two orgs switches between them, and each shows its own work')
  await page.goto(`${UI}/platform-experiments`)
  await page.getByText(experimentName).first().waitFor()
  await page.route(`${API}/orgs`, route => route.abort())
  await createInNav(page, 'organization', `${ada.name} Second`)
  await page.locator('.wa-nav-create .text-error').waitFor()
  await shot(page, 'new-organization-api-failing')
  await page.unroute(`${API}/orgs`)
  await page.locator('.wa-nav-create').getByRole('button', { name: 'Create' }).dblclick()
  await page.waitForURL(/\/platform-experiments\?project=/)
  const adaOrgs = (await (await api(await tokenOf(ada), '/me')).json()).orgs
  assert.equal(adaOrgs.filter(o => o.name === `${ada.name} Second`).length, 1, 'a double click created one org')
  assert.equal(await page.locator('.wa-nav-tenant').getByRole('combobox').first().inputValue(), `${ada.name} Second`, 'the new org is selected')
  assert.equal(await page.locator('.wa-nav-tenant').getByRole('combobox').nth(1).inputValue(), 'default')
  await page.getByText(`${ada.name} Second / default has no platform experiments yet`).waitFor()
  assert.equal(await page.getByText(experimentName).count(), 0)
  await shot(page, 'second-org-is-empty')
  await page.getByRole('link', { name: 'Settings' }).click()
  await page.getByRole('heading', { name: 'Settings' }).waitFor()
  assert.equal(await page.getByRole('cell', { name: 'laptop' }).count(), 0, 'the other org\'s API key is not listed')
  assert.equal(await page.getByRole('cell', { name: clusterName }).count(), 0, 'the other org\'s cluster is not listed')
  await page.getByLabel('Organization name').fill(`${ada.name} Lab`)
  await page.getByLabel('Organization name').locator('..').getByRole('button', { name: 'Rename' }).click()
  await page.waitForFunction(name => document.querySelector('.wa-nav-tenant input')?.value.startsWith(`${name} (`), `${ada.name} Lab`)
  await page.locator('.wa-nav-tenant').getByRole('combobox').first().click()
  assert.equal(await page.getByRole('option', { name: new RegExp(`^${ada.name} Lab \\(`) }).count(), 2, 'two orgs of one name are told apart')
  await shot(page, 'two-orgs-same-name')
  await page.keyboard.press('Escape')
  await page.getByLabel('Organization name').fill(`${ada.name} Second`)
  await page.getByLabel('Organization name').locator('..').getByRole('button', { name: 'Rename' }).click()
  await page.waitForFunction(name => document.querySelector('.wa-nav-tenant input')?.value === name, `${ada.name} Second`)
  await selectInNav(page, 'Organization', `${ada.name} Lab`)
  await page.getByRole('cell', { name: new RegExp(clusterName) }).waitFor()

  section('switching org on Settings takes the last org\'s token and half-typed form off the screen')
  await page.getByLabel('Key name').fill('on-show')
  await page.getByRole('button', { name: 'Create API key' }).click()
  const shownKey = await page.getByTestId('secret').innerText()
  assert.match(shownKey, /^hlk_/)
  secrets.add(shownKey)
  await page.getByRole('button', { name: 'Show start command' }).click()
  await page.getByTestId('secret').nth(1).waitFor()
  await page.getByLabel('Cluster name').fill('half-typed')
  await shot(page, 'token-on-show-before-switch', page.getByTestId('secret').first())
  await selectInNav(page, 'Organization', `${ada.name} Second`)
  await page.getByText(`${ada.name} Second: its name, projects, API keys and clusters.`).waitFor()
  assert.equal(await page.getByTestId('secret').count(), 0, 'the other org\'s key and start command are gone')
  assert.ok(!(await page.content()).includes(shownKey), 'the other org\'s API key is nowhere on the page')
  assert.equal(await page.getByLabel('Cluster name').inputValue(), '', 'the half-typed cluster name is gone')
  await shot(page, 'token-gone-after-switch')
  await selectInNav(page, 'Organization', `${ada.name} Lab`)
  await page.getByRole('cell', { name: new RegExp(clusterName) }).waitFor()

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
  const other = await newContext(browser)
  const otherPage = await other.newPage()
  await otherPage.goto(`${UI}/platform-experiments`)
  await signIn(otherPage, bob, `${UI}/platform-experiments`)
  await otherPage.getByText('has no platform experiments yet').waitFor()
  assert.equal(await otherPage.getByText(experimentName).count(), 0)
  await shot(otherPage, 'second-account-sees-nothing')
  const foreignRead = otherPage.waitForResponse(r => r.url() === `${API}/platform-experiments/${experimentID}`)
  await otherPage.goto(`${UI}/platform-experiments/${experimentID}`)
  const foreign = await foreignRead
  assert.equal(foreign.status(), 404, 'the API answers another org\'s experiment as not found')
  const refusal = await foreign.json()
  await otherPage.locator('.error-banner').waitFor()
  assert.equal((await otherPage.locator('.error-banner').innerText()).trim(), `Could not load this platform experiment: ${refusal.message ?? refusal.error}`)
  assert.ok(!(await otherPage.content()).includes(experimentName), 'the experiment\'s name is nowhere on the page')
  await shot(otherPage, 'second-account-opens-foreign-experiment')
  await otherPage.goto(`${UI}/jobs?project=${adaMe.projects[0].id}`)
  await otherPage.getByText('is not available to you').waitFor()
  await shot(otherPage, 'foreign-project-in-url')
  await otherPage.getByRole('button', { name: /^Go to / }).click()
  await otherPage.getByRole('heading', { name: 'Jobs' }).waitFor()

  section('a member of an org sees its work and can change nothing about the org')
  const bobID = (await (await api(await tokenOf(bob), '/me')).json()).user_id
  const labID = adaMe.orgs[0].id
  for (const id of [bobID, labID]) assert.match(id, /^[\w-]+$/)
  sql(`INSERT INTO org_members (org_id, user_id, role) VALUES ('${labID}', '${bobID}', 'member');`)
  try {
    await otherPage.goto(`${UI}/settings?project=${adaMe.projects[0].id}`)
    await otherPage.getByText(`You are a member of ${ada.name} Lab. Only an owner can`).waitFor()
    assert.equal(await otherPage.getByLabel('Organization name').count(), 0)
    assert.equal(await otherPage.getByRole('button', { name: 'Create API key' }).count(), 0)
    assert.equal(await otherPage.getByRole('button', { name: 'Add cluster' }).count(), 0)
    assert.equal(await otherPage.getByRole('button', { name: '+ New project' }).count(), 0)
    await shot(otherPage, 'member-settings')
    const settingsReads = []
    otherPage.on('request', r => { if (/\/clusters\/[^/]+\/settings$/.test(r.url())) settingsReads.push(r.url()) })
    await otherPage.getByRole('link', { name: 'Compute Resources' }).click()
    const memberRow = otherPage.getByRole('row', { name: new RegExp(clusterName) })
    await memberRow.getByText(`set by an owner of ${ada.name} Lab`).waitFor()
    assert.equal(await otherPage.getByRole('button', { name: 'Edit' }).count(), 0, 'a member is offered no settings editor')
    assert.deepEqual(settingsReads, [], 'and the page asks for no settings the API would refuse')
    assert.equal((await api(await tokenOf(bob), `/clusters/${clusterID}/settings`)).status, 403, 'which it does refuse')
    await shot(otherPage, 'member-cluster-page', memberRow)
    await otherPage.getByRole('link', { name: 'Platform Experiments' }).click()
    await otherPage.getByText(`${experimentName}-edited`).first().waitFor()
  } finally {
    sql(`DELETE FROM org_members WHERE org_id = '${labID}' AND user_id = '${bobID}' AND role = 'member';`)
  }

  section('staff see the views across every org, and a customer does not')
  hlAdmin('user', 'staff', bob.id, 'on')
  try {
    await otherPage.goto(`${UI}/cluster`)
    await otherPage.getByText(`Clusters ${bob.name} can run on`).waitFor()
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

  section('a page left open past the expiry of its token goes on working with refreshed ones')
  // supabase-js replaces a token within 90 seconds of its expiry, so only a longer lifetime
  // has the page use one token and then move to the next while it stays open.
  const refreshesBefore = (await idpState()).refreshes
  await configure({ tokenLifetimeSeconds: 100 })
  const shortLived = await newContext(browser)
  const shortPage = await shortLived.newPage()
  const sent = []
  const statuses = []
  shortPage.on('request', r => { if (r.url().startsWith(API) && r.method() !== 'OPTIONS') sent.push(r.headers().authorization.slice(7)) })
  shortPage.on('response', r => { if (r.url().startsWith(API)) statuses.push(r.status()) })
  await shortPage.goto(`${UI}/jobs`)
  await signIn(shortPage, eve, `${UI}/jobs`)
  await shortPage.getByRole('heading', { name: 'Jobs' }).waitFor()
  const firstToken = sent[0]
  assert.equal((await api(firstToken, '/me')).status, 200, 'the first token works while it lasts')
  // From here the page is not touched: its own polling is what crosses the expiry.
  const expiryDeadline = Date.now() + 130_000
  while ((await api(firstToken, '/me')).status !== 401) {
    assert.ok(Date.now() < expiryDeadline, 'the first token never expired')
    await shortPage.waitForTimeout(5_000)
  }
  const sentBeforeExpiry = sent.length
  await shortPage.waitForTimeout(15_000)
  const sentAfterExpiry = sent.slice(sentBeforeExpiry)
  assert.ok(sentAfterExpiry.length > 0, 'the page went on asking the API after its first token expired')
  assert.ok(!sentAfterExpiry.includes(firstToken), 'and never with the expired token')
  assert.ok(new Set(sent).size > 1, 'requests moved to a new token')
  assert.ok((await idpState()).refreshes > refreshesBefore, 'the session was refreshed')
  assert.ok(!statuses.includes(401), 'no request of the page was refused across the expiry')
  assert.equal(shortPage.url(), `${UI}/jobs`, 'the page never left')
  assert.equal(await shortPage.locator('.error-banner').count(), 0)

  section('a session that can no longer be refreshed ends at the sign-in page, with the way back')
  // supabase-js keeps a session whose refresh was refused for as long as its access token is
  // still good, so the session ends when that token does, up to its whole lifetime from now.
  await configure({ revoke: eve.id })
  await shortPage.waitForURL(/\/login\?notice=ended&next=/, { timeout: 150_000 })
  await shortPage.getByText('Your session ended.').waitFor()
  await shot(shortPage, 'session-ended')
  await shortLived.close()

  section('a token the API refuses ends at the sign-in page and does not loop')
  await configure({ tokenLifetimeSeconds: 3600, audience: 'some-other-api' })
  const refused = await newContext(browser)
  const refusedPage = await refused.newPage()
  await refusedPage.goto(`${UI}/agents`)
  await signIn(refusedPage, eve, /\/login\?notice=refused&next=%2Fagents/)
  await refusedPage.getByText('The API did not accept your session.').waitFor()
  await refusedPage.waitForTimeout(1_500)
  assert.match(refusedPage.url(), /\/login\?notice=refused/, 'the page stays on sign-in')
  await shot(refusedPage, 'session-refused')
  await refused.close()
  await configure({ audience: 'hypothesisloop-dev' })

  await context.close()
  assert.deepEqual(crashes, [], 'no page threw an uncaught error')
  assert.ok(secrets.size > 5 && requested.length > 100)
  for (const url of requested) {
    assert.notEqual(new URL(url).hostname, 'evil.example', 'nothing was asked of the site a hostile way back named')
    for (const secret of secrets) assert.ok(!url.includes(secret), `a token is in the URL ${url.replace(secret, '<token>')}`)
  }
}

main().catch(error => {
  console.error(error)
  process.exit(1)
})

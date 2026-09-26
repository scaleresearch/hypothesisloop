import assert from 'node:assert/strict'
import { execFileSync, spawn, spawnSync } from 'node:child_process'
import { randomUUID } from 'node:crypto'
import { once } from 'node:events'
import { existsSync, mkdirSync, readFileSync, rmSync } from 'node:fs'
import { dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'
import { chromium } from 'playwright'
import { startIdentityProvider } from './identity-provider.mjs'
import { startProxy } from './proxy.mjs'

const here = dirname(fileURLToPath(import.meta.url))
const repo = join(here, '..', '..', '..')
const shots = join(here, 'screenshots')
// The one origin the API allows a browser from and the GitHub App returns to. Port 3000 itself
// belongs to the UI the stack's owner runs: the run serves its own copy on UI_PORT, and its
// browser reaches that copy under this origin through the proxy.
const UI = 'http://localhost:3000'
const UI_PORT = 3100
const PROXY_PORT = 54398
const UI_BUILD = '.next-e2e'
const API = 'http://localhost:8081'
const IDP = 'http://127.0.0.1:54399'
const STACK_LOCK = '/tmp/hl-stack.lock'
// hl-admin reads the settings the running control plane was started with.
const SETTINGS = join(repo, 'controlplane', 'settings', '.hypothesisloop.dev.yaml')
const run = randomUUID().slice(0, 8)

// Reduced motion turns the fade-in of panels off, so a screenshot shows them as they end up.
const VIEW = { viewport: { width: 1440, height: 900 }, reducedMotion: 'reduce' }

const person = first => ({ id: randomUUID(), name: `UI E2E ${first} ${run}` })
const ada = person('Ada')
const bob = person('Bob')
const eve = person('Eve')
const dan = person('Dan')

async function configure(settings) {
  const res = await fetch(`${IDP}/test/configure`, { method: 'POST', body: JSON.stringify(settings) })
  assert.equal(res.status, 200)
}

async function idpState() {
  return (await fetch(`${IDP}/test/state`)).json()
}

// Everyone the stand-in issued a token for. The API gives each a home org at their first request,
// so these are the accounts the run leaves behind and cleans up after.
const people = new Map()

// A token of the user straight from the identity provider, for reading back what a page did.
async function tokenOf(user) {
  people.set(user.id, user)
  const res = await fetch(`${IDP}/test/token`, { method: 'POST', body: JSON.stringify({ user }) })
  return (await res.json()).access_token
}

async function api(token, path, method = 'GET', body) {
  return fetch(`${API}${path}`, {
    method,
    headers: { Authorization: `Bearer ${token}`, ...(body === undefined ? {} : { 'Content-Type': 'application/json' }) },
    body: body === undefined ? undefined : JSON.stringify(body),
  })
}

// The platform experiments the run had staff share, taken back in the cleanup.
const sharedByRun = new Set()

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
  execFileSync('bash', ['-c', '. localdev/lib/auth.sh && lib_hl_admin "$@"', 'hl-admin', ...args], {
    cwd: repo, stdio: 'inherit', env: { ...process.env, HYPOTHESISLOOP_CONFIG: SETTINGS },
  })
}

// Sharing is staff's alone and has no control in the UI: it is done as staff do it.
function share(platformExperimentID) {
  sharedByRun.add(platformExperimentID)
  hlAdmin('platform-experiment', 'share', platformExperimentID)
}

function unshare(platformExperimentID) {
  hlAdmin('platform-experiment', 'unshare', platformExperimentID)
  sharedByRun.delete(platformExperimentID)
}

// A project's list of platform experiments with none of the project's own in it: the empty
// state, or on a stack whose owner shares some, those and nothing else.
function noneOfItsOwn(page, emptyState = 'None yet. Write what you want to find out above') {
  return page.getByText(emptyState).or(page.getByText('Shared · read only').first()).first()
}

const listening = port => fetch(`http://localhost:${port}`).then(() => true, () => false)

// The run creates platform experiments and adds agents to them. On a stack that rents compute
// (make dev-hosted) a hosted one would be rented for real, so only the local stack is run against.
async function requireLocalStack() {
  if (spawnSync('bash', ['localdev/dev.sh', 'require-local'], { cwd: repo, stdio: 'inherit' }).status !== 0) {
    throw new Error('the browser run needs the stack make dev runs, which rents nothing')
  }
  if (existsSync(STACK_LOCK)) {
    const owner = existsSync(join(STACK_LOCK, 'owner')) ? readFileSync(join(STACK_LOCK, 'owner'), 'utf8').trim() : 'no owner named'
    throw new Error(`the stack is held (${STACK_LOCK}): ${owner}`)
  }
  const health = await fetch(`${API}/health`).catch(() => null)
  if (!health?.ok) throw new Error(`no control plane answers at ${API}; start it with make dev`)
  for (const port of [UI_PORT, PROXY_PORT]) {
    if (await listening(port)) throw new Error(`port ${port} is already serving something, and the browser run needs it`)
  }
}

// Asked in node, so at the port the copy listens on: only the browser goes through the proxy.
async function waitForUI(ui) {
  for (let i = 0; i < 120; i++) {
    if (ui.exitCode !== null || ui.signalCode !== null) throw new Error(`next dev stopped (${ui.exitCode ?? ui.signalCode}) before it served anything`)
    const login = await fetch(`http://localhost:${UI_PORT}/login`).catch(() => null)
    if (login?.ok) return
    await new Promise(resolve => setTimeout(resolve, 1000))
  }
  throw new Error(`the UI did not come up on port ${UI_PORT}`)
}

// Leaves the stack without what the run created, through the API as the people who created it.
// Every one of them is new in this run, so every org one of them owns is the run's own. An open
// platform experiment is closed, a cluster removed, an API key and an invite revoked, a GitHub
// account disconnected and every other member removed. The API deletes no org, project or closed
// platform experiment, so those stay. An experiment the run shared is taken back first: one left
// shared would be in every account's list on the stack.
async function cleanUp() {
  await configure({ tokenLifetimeSeconds: 3600, audience: 'hypothesisloop-dev' })
  const failures = []
  const attempt = async (what, act) => {
    try {
      await act()
    } catch (error) {
      failures.push(`${what}: ${error.message}`)
    }
  }
  const read = async (token, path) => {
    const res = await api(token, path)
    if (!res.ok) throw new Error(`GET ${path} answered ${res.status} ${await res.text()}`)
    return res.json()
  }
  const change = (token, method, path) => attempt(`${method} ${path}`, async () => {
    const res = await api(token, path, method)
    if (!res.ok) throw new Error(`answered ${res.status} ${await res.text()}`)
  })
  for (const id of sharedByRun) {
    await attempt(`unsharing platform experiment ${id}`, async () => unshare(id))
  }
  for (const user of people.values()) {
    await attempt(`cleaning up after ${user.name}`, async () => {
      const token = await tokenOf(user)
      const me = await read(token, '/me')
      for (const org of me.orgs.filter(o => o.role === 'owner')) {
        for (const project of me.projects.filter(p => p.org_id === org.id)) {
          await attempt(`platform experiments of project ${project.id}`, async () => {
            for (const status of ['open', 'running']) {
              for (const pe of await read(token, `/platform-experiments?project_id=${project.id}&status=${status}&limit=200`)) {
                await change(token, 'POST', `/platform-experiments/${pe.id}/close`)
              }
            }
          })
        }
        await attempt(`clusters of org ${org.id}`, async () => {
          for (const cluster of (await read(token, `/orgs/${org.id}/clusters`)).clusters.filter(c => c.owned)) {
            await change(token, 'DELETE', `/orgs/${org.id}/clusters/${cluster.name}`)
          }
        })
        await attempt(`API keys of org ${org.id}`, async () => {
          for (const key of (await read(token, `/orgs/${org.id}/api-keys`)).api_keys.filter(k => !k.revoked_at)) {
            await change(token, 'DELETE', `/api-keys/${key.id}`)
          }
        })
        await attempt(`invites of org ${org.id}`, async () => {
          for (const invite of await read(token, `/orgs/${org.id}/invites?limit=200`)) await change(token, 'DELETE', `/invites/${invite.id}`)
        })
        await attempt(`GitHub accounts of org ${org.id}`, async () => {
          for (const installation of await read(token, `/orgs/${org.id}/github-installations?limit=200`)) {
            await change(token, 'DELETE', `/orgs/${org.id}/github-installations/${installation.installation_id}`)
          }
        })
        await attempt(`members of org ${org.id}`, async () => {
          for (const member of (await read(token, `/orgs/${org.id}/members?limit=200`)).filter(m => m.user_id !== me.user_id)) {
            await change(token, 'DELETE', `/orgs/${org.id}/members/${member.user_id}`)
          }
        })
      }
    })
  }
  if (failures.length > 0) throw new Error(`the run left something behind on the stack:\n  ${failures.join('\n  ')}`)
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

// A confirmation is a dialog on the page; `button` is the one that goes ahead.
async function confirmWith(page, button) {
  const question = page.getByRole('dialog')
  await question.getByRole('button', { name: button, exact: true }).click()
  await question.waitFor({ state: 'detached' })
}

// Picks an option of a Select: its field is a button that opens a listbox.
async function choose(page, field, option) {
  await field.click()
  await page.getByRole('option', { name: option, exact: true }).click()
}

// Clicks the GitHub sign-in button and follows the redirects back into the app.
async function signIn(page, user, landing = `${UI}/platform-experiments`) {
  people.set(user.id, user)
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
  await requireLocalStack()
  rmSync(shots, { recursive: true, force: true })
  mkdirSync(shots, { recursive: true })
  const idp = await startIdentityProvider({ port: 54399, keyPath: join(repo, 'localdev', '.dev-jwt-key.pem'), ui: UI })
  const closers = [() => idp.close()]
  try {
    // Asked as the first person a scenario signs in, so the question leaves no account of its own.
    if ((await api(await tokenOf(eve), '/me')).status !== 200) {
      throw new Error(`the API at ${API} does not accept a person signed with localdev/.dev-jwt-key.pem: restart it with make dev, which lists that key in auth.identity_providers`)
    }
    try {
      await play(closers)
      console.log('\nbrowser run passed')
    } finally {
      // A failed cleanup fails the run and leaves the scenario's own error to be the one thrown.
      await cleanUp().catch(error => {
        console.error(error)
        process.exitCode = 1
      })
    }
  } finally {
    for (const close of closers.reverse()) await close()
  }
}

// Serves the run's copy of the UI, opens a browser that reaches it as UI, and plays the scenarios.
async function play(closers) {
  const [host, port] = new URL(UI).host.split(':')
  const proxy = await startProxy({ port: PROXY_PORT, from: { host, port: Number(port) }, to: { host, port: UI_PORT } })
  closers.push(() => proxy.close())
  const ui = spawn(process.execPath, [join(here, '..', 'node_modules', 'next', 'dist', 'bin', 'next'), 'dev', '-p', String(UI_PORT)], {
    cwd: join(here, '..'),
    env: {
      ...process.env,
      NEXT_DIST_DIR: UI_BUILD,
      NEXT_PUBLIC_API_URL: API,
      NEXT_PUBLIC_SUPABASE_URL: IDP,
      NEXT_PUBLIC_SUPABASE_PUBLISHABLE_KEY: 'sb_publishable_browser-run',
      NEXT_PUBLIC_SIGN_IN_PROVIDERS: 'github,google,apple',
    },
    stdio: ['ignore', 'ignore', 'inherit'],
  })
  // The build goes with the server: tsconfig lists its generated types, and ones left behind
  // would fail the next typecheck once a page they name has moved.
  closers.push(async () => {
    if (ui.exitCode === null && ui.signalCode === null) {
      ui.kill()
      await once(ui, 'exit')
    }
    rmSync(join(here, '..', UI_BUILD), { recursive: true, force: true, maxRetries: 5, retryDelay: 200 })
  })
  // Chromium reaches loopback addresses directly unless told that they go through the proxy too.
  const browser = await chromium.launch({ proxy: { server: `http://127.0.0.1:${PROXY_PORT}`, bypass: '<-loopback>' } })
  closers.push(() => browser.close())
  try {
    await waitForUI(ui)
    await scenarios(browser)
  } catch (error) {
    for (const context of browser.contexts()) {
      for (const page of context.pages()) await shot(page, 'failure')
    }
    throw error
  }
}

async function scenarios(browser) {
  section('the landing page is public and offers sign-in and sign-up')
  const visitor = await newContext(browser)
  const visitorPage = await visitor.newPage()
  await visitorPage.goto(UI)
  await visitorPage.getByRole('heading', { level: 1 }).waitFor()
  assert.equal(new URL(visitorPage.url()).pathname, '/', 'a visitor without a session stays on the landing page')
  assert.equal(await visitorPage.getByRole('link', { name: 'Experiments', exact: true }).count(), 0)
  await shot(visitorPage, 'landing-signed-out')
  await visitorPage.getByRole('link', { name: 'Sign up' }).click()
  await visitorPage.waitForURL(`${UI}/login?mode=sign-up`)
  await visitorPage.getByRole('heading', { name: 'Create your account' }).waitFor()
  await shot(visitorPage, 'sign-up')
  await visitorPage.getByRole('link', { name: 'Sign in' }).click()
  await visitorPage.waitForURL(`${UI}/login`)
  await visitorPage.getByRole('heading', { name: 'Sign in' }).waitFor()

  section('a provider the identity provider has switched off says so and leaves the others')
  assert.deepEqual(
    await visitorPage.getByRole('button').allInnerTexts(),
    ['Continue with GitHub', 'Continue with Google', 'Continue with Apple'],
    'the sign-in page offers the configured providers',
  )
  await visitorPage.getByRole('button', { name: 'Continue with Google' }).click()
  await visitorPage.locator('.error-banner', { hasText: 'Sign-in with Google is not available yet.' }).waitFor()
  assert.equal(visitorPage.url(), `${UI}/login`, 'the visitor is still on the sign-in page')
  assert.ok(await visitorPage.getByRole('button', { name: 'Continue with Google' }).isDisabled())
  assert.ok(await visitorPage.getByRole('button', { name: 'Continue with GitHub' }).isEnabled())
  await shot(visitorPage, 'sign-in-provider-refused')

  section('a sign-in that does not complete says so')
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
  await signIn(visitorPage, eve)
  assert.equal(new URL((await idpState()).authorizeRequests.at(-1)).searchParams.get('redirect_to'), `${UI}/auth/callback?next=%2Fconsole`)
  for (const next of HOSTILE) {
    await visitorPage.goto(`${UI}/login?next=${next}`)
    await visitorPage.waitForURL(`${UI}/platform-experiments`)
  }

  section('the landing page offers a signed-in visitor the experiments')
  await visitorPage.goto(UI)
  assert.equal(await visitorPage.getByRole('link', { name: 'Sign in' }).count(), 0)
  await shot(visitorPage, 'landing-signed-in')
  await visitorPage.getByRole('link', { name: 'Experiments', exact: true }).click()
  await visitorPage.waitForURL(`${UI}/platform-experiments`)
  await visitor.close()

  const context = await newContext(browser)
  await context.grantPermissions(['clipboard-read', 'clipboard-write'], { origin: UI })
  const page = await context.newPage()
  const apiCalls = []
  page.on('request', request => {
    if (request.url().startsWith(API) && request.method() !== 'OPTIONS') apiCalls.push(request)
  })

  section('a visitor without a session is sent to sign in, and returns to where they were going')
  await page.goto(`${UI}/jobs?status=RUNNING`)
  await page.waitForURL(`${UI}/login?next=%2Fjobs%3Fstatus%3DRUNNING`)
  await page.getByRole('button', { name: 'Continue with GitHub' }).waitFor()
  assert.equal(await page.getByRole('button').count(), 3, 'the login page has one button for each configured provider')
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

  section('a first-time user lands in their own org and default project, on one sentence to start from')
  await page.goto(`${UI}/platform-experiments`)
  await page.getByText(`New experiment in ${ada.name} / default`).waitFor()
  await page.getByLabel('What do you want to find out?').waitFor()
  await page.getByText('None yet. Write what you want to find out above').waitFor()
  assert.equal(await page.getByPlaceholder('e.g. Q3 Vision Benchmark').count(), 0, 'the full form is behind Advanced')
  await page.locator('.wa-nav-tenant').getByRole('combobox').first().waitFor()
  assert.equal(await page.locator('.wa-nav-tenant').getByRole('combobox').first().inputValue(), 'Default org')
  assert.equal(await page.locator('.wa-nav-tenant').getByRole('combobox').nth(1).inputValue(), 'Default project')
  await shot(page, 'first-run-home')
  await page.getByRole('link', { name: 'Experiments', exact: true }).click()
  await noneOfItsOwn(page).waitFor()
  await shot(page, 'first-run-empty-project')

  section('creating a platform experiment in the selected project')
  await page.getByRole('button', { name: 'Advanced', exact: true }).click()
  const experimentName = `ui-e2e-${run}`
  await page.getByPlaceholder('e.g. Q3 Vision Benchmark').fill(experimentName)
  await choose(page, page.getByLabel('Common metric'), 'Val Accuracy (↑ max)')
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
  const agentAdds = []
  page.on('request', request => {
    if (request.method() === 'POST' && new URL(request.url()).pathname.endsWith('/agents')) agentAdds.push(request)
  })
  await page.getByPlaceholder("agent's default").first().fill('hyperparameter-search')
  await page.getByRole('button', { name: 'Me', exact: true }).click()
  await page.getByRole('button', { name: 'Add agent' }).click()
  await page.getByTestId('secret').waitFor()
  await page.getByRole('button', { name: 'Add agent' }).click()
  await page.getByTestId('secret').nth(1).waitFor()
  assert.deepEqual(agentAdds.map(r => r.postDataJSON()), Array(2).fill({ hosted: false, flavor: 'hyperparameter-search' }), 'each agent is one call that names no count')
  const agentEnv = await page.getByTestId('secret').first().innerText()
  // An agent is started with the API's address and its token, and nothing else.
  assert.match(agentEnv, /^API_URL=http\S+\nHL_TOKEN=hls_\S+$/)
  await shot(page, 'self-run-agent-tokens', page.getByTestId('secret').first())
  const agentToken = agentEnv.match(/HL_TOKEN=(\S+)/)[1]
  const experimentID = new URL(experimentURL).pathname.split('/').pop()
  // The token says which agent of which platform experiment it is.
  const [tokenExperiment, agentID] = Buffer.from(agentToken.slice('hls_'.length).split('.')[0], 'base64url').toString().replace(/^agent:/, '').split('/')
  assert.equal(tokenExperiment, experimentID)
  assert.match(agentID, /^ag-/)
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
  const agentRows = page.locator('table', { hasText: 'Run by' }).locator('tbody tr')
  assert.equal(await agentRows.count(), 2)

  section('a hosted agent the platform refuses is not listed, and the page says why')
  // An experiment needs no repository to have its agents hosted, so the real API would take
  // this one and a launcher would rent for it. The refusal is stood in for: what a deployment
  // with no launcher registered answers. Nothing reaches the API.
  const refusedHosting = { reason: 'launcher_not_registered', message: 'no launcher named main is registered: nothing can be hosted until hl-admin launcher add main' }
  const hostedAddURL = `${API}/platform-experiments/${experimentID}/agents`
  const refuseHosted = route => route.request().method() === 'POST'
    ? route.fulfill({ status: 503, headers: { 'access-control-allow-origin': UI }, json: refusedHosting })
    : route.fallback()
  await page.route(hostedAddURL, refuseHosted)
  const hostedAdd = page.waitForResponse(r => r.request().method() === 'POST' && r.url() === hostedAddURL)
  await page.getByRole('button', { name: 'The platform' }).click()
  await page.getByRole('button', { name: 'Add agent' }).click()
  assert.deepEqual((await hostedAdd).request().postDataJSON().hosted, true, 'the agent was asked for hosted')
  const hostedRefusal = page.locator('.error-banner', { hasText: 'no launcher named main is registered' })
  await hostedRefusal.waitFor()
  await page.unroute(hostedAddURL, refuseHosted)
  assert.equal(await agentRows.count(), 2, 'the refused agent is not listed')
  await shot(page, 'hosted-agent-refused', hostedRefusal)

  section('removing an agent ends its token')
  assert.equal((await api(agentToken, `/platform-experiments/${experimentID}`)).status, 200, 'the agent token still works before its agent is removed')
  await agentRows.filter({ hasText: agentID }).getByRole('button', { name: 'Remove' }).click()
  await confirmWith(page, 'Remove')
  await agentRows.filter({ hasText: agentID }).waitFor({ state: 'detached' })
  assert.equal(await agentRows.count(), 1)
  assert.equal((await api(agentToken, `/platform-experiments/${experimentID}`)).status, 401, 'the same request is refused once the agent is removed')
  await shot(page, 'experiment-agents', agentRows.last())

  section('a hosted agent shows what became of its machine')
  // Stood in for: an agent the platform really hosts needs a repository and a launcher that rents.
  const agentsAPI = `${API}/platform-experiments/${experimentID}/agents`
  const signedUp = { platform_experiment_id: experimentID, role: 'competitor', hosted: true, flavor: 'generalist', model: 'stand-in-model', signed_up_at: new Date().toISOString() }
  await page.route(agentsAPI, route => route.request().method() === 'GET'
    ? route.fulfill({
      headers: { 'access-control-allow-origin': UI },
      json: {
        agents: [
          { ...signedUp, agent_id: 'ag-hosted-running', name: 'ag-hosted-running', instance: { state: 'running', restarts: 1, stopped: false, progress_at: new Date().toISOString(), end_reason: 'exited', exited_at: new Date().toISOString() } },
          { ...signedUp, agent_id: 'ag-hosted-stopped', name: 'ag-hosted-stopped', instance: { state: 'stopped', restarts: 3, stopped: true, end_reason: 'silent', exited_at: new Date().toISOString(), exit_log: 'the last lines it wrote' } },
          { ...signedUp, agent_id: 'ag-hosted-new', name: 'ag-hosted-new' },
        ],
      },
    })
    : route.fallback())
  await page.reload()
  const runningRow = agentRows.filter({ hasText: 'ag-hosted-running' })
  await runningRow.getByText('running', { exact: true }).waitFor()
  await runningRow.getByText('restarts 1 of 3').waitFor()
  await runningRow.getByText(/^last progress /).waitFor()
  await runningRow.getByText(/^Replaced: the machine before this one ended .*, its command ended\.$/).waitFor()
  assert.equal(await runningRow.getByRole('button', { name: 'Show token' }).count(), 0, 'a hosted agent has no token to show')
  const stoppedRow = agentRows.filter({ hasText: 'ag-hosted-stopped' })
  await stoppedRow.getByText('stopped', { exact: true }).waitFor()
  await stoppedRow.getByText(/^Stopped for good: its last machine ended .*, it ran and made no progress\.$/).waitFor()
  await stoppedRow.getByRole('button', { name: 'Exit log' }).click()
  await stoppedRow.getByText('the last lines it wrote').waitFor()
  await agentRows.filter({ hasText: 'ag-hosted-new' }).getByText('not reported yet').waitFor()
  await shot(page, 'hosted-agent-health', stoppedRow)
  await page.unroute(agentsAPI)

  section('home: Advanced opens the full form in a dialog, and the new experiment is created and started from there')
  await page.goto(`${UI}/platform-experiments`)
  await page.getByRole('heading', { name: 'Experiments', exact: true }).waitFor()
  await page.getByText(experimentName).first().waitFor()
  assert.equal(await page.getByPlaceholder('e.g. Q3 Vision Benchmark').count(), 0, 'the form is behind Advanced')
  await shot(page, 'home-with-experiments')
  await page.getByRole('button', { name: 'Advanced', exact: true }).click()
  const homeDialog = page.getByRole('dialog')
  const homeExperimentName = `ui-e2e-home-${run}`
  await homeDialog.getByPlaceholder('e.g. Q3 Vision Benchmark').fill(homeExperimentName)
  await choose(page, homeDialog.getByLabel('Common metric'), 'Val Accuracy (↑ max)')
  await homeDialog.getByRole('button', { name: 'Add', exact: true }).click()
  await homeDialog.getByRole('button', { name: 'Next: competition settings' }).click()
  const homeCreated = page.waitForResponse(r => r.url() === `${API}/platform-experiments` && r.request().method() === 'POST')
  await homeDialog.getByRole('button', { name: 'Create Experiment' }).click()
  assert.equal((await homeCreated).status(), 201)
  assert.equal((await homeCreated).request().postDataJSON().project_id, adaMe.projects[0].id, 'in the project the switcher shows')
  const homeExperimentID = (await (await homeCreated).json()).id
  await page.waitForURL(u => u.pathname === `/platform-experiments/${homeExperimentID}`)
  await page.getByRole('heading', { name: homeExperimentName }).waitFor()
  // Starting with nobody in it is the API's to refuse, in its own words.
  const refusedStart = page.waitForResponse(r => r.url() === `${API}/platform-experiments/${homeExperimentID}/start`)
  await page.getByRole('button', { name: 'Start now' }).click()
  await confirmWith(page, 'Start')
  assert.equal((await refusedStart).status(), 400)
  await page.locator('.error-banner').first().waitFor()
  await shot(page, 'home-experiment-start-refused', page.locator('.error-banner').first())
  // A self-run agent: the platform rents nothing for it.
  await page.getByRole('button', { name: 'Me', exact: true }).click()
  await page.getByRole('button', { name: 'Add agent' }).click()
  await page.getByTestId('secret').waitFor()
  secrets.add((await page.getByTestId('secret').innerText()).match(/HL_TOKEN=(\S+)/)[1])
  const started = page.waitForResponse(r => r.url() === `${API}/platform-experiments/${homeExperimentID}/start`)
  await page.getByRole('button', { name: 'Start now' }).click()
  await confirmWith(page, 'Start')
  assert.equal((await started).status(), 200)
  await page.getByText('The experiment is running:').waitFor()
  assert.equal(await page.getByRole('button', { name: 'Start now' }).count(), 0, 'a started experiment is not started again')
  assert.equal((await (await api(await tokenOf(ada), `/platform-experiments/${homeExperimentID}`)).json()).status, 'running')
  await shot(page, 'home-experiment-started')

  section('home: Create and run composes create, one call an agent, and start, and nothing is rented')
  // The stack has no GitHub account connected, and an agent the platform really hosts has a
  // launcher rent for it. So the repository list is stood in for, and each write goes to
  // the real API with what would rent taken out of it: the repository and the hosted coordinator
  // from the create, and hosted from each agent, which then joins as one its operator runs. What
  // the page sent is asserted as sent, and what exists afterwards is asserted from the real API.
  const runCors = { 'access-control-allow-origin': UI, 'access-control-expose-headers': 'X-Total-Count' }
  const runInstallations = url => url.href.startsWith(`${API}/orgs/${adaMe.orgs[0].id}/github-installations`)
  const runRepositories = url => url.href.startsWith(`${API}/orgs/${adaMe.orgs[0].id}/github-repositories`)
  await page.route(runInstallations, route => route.request().method() === 'GET'
    ? route.fulfill({
      headers: { ...runCors, 'X-Total-Count': '1' },
      json: [{ org_id: adaMe.orgs[0].id, installation_id: 4242, account_login: 'acme', connected_by: adaMe.user_id, connected_by_name: ada.name, connected_at: new Date().toISOString() }],
    })
    : route.fallback())
  await page.route(runRepositories, route => route.fulfill({
    headers: { ...runCors, 'X-Total-Count': '1' }, json: [{ repository: 'acme/widgets', installation_id: 4242, private: false }],
  }))
  const runWrites = []
  let standInAgents = true
  const runWrite = url => url.href === `${API}/platform-experiments` || /\/platform-experiments\/[^/]+\/(agents|start)$/.test(url.pathname)
  await page.route(runWrite, route => {
    const request = route.request()
    if (request.method() !== 'POST') return route.fallback()
    const path = new URL(request.url()).pathname
    const body = request.postDataJSON()
    runWrites.push({ path, body })
    if (path === '/platform-experiments') {
      const { repository: _repository, coordinator_hosted: _coordinator, ...rest } = body
      return route.continue({ postData: JSON.stringify(rest) })
    }
    if (path.endsWith('/agents')) {
      // Never hosted for real: joined as one its operator runs, or refused as a deployment
      // with no launcher registered refuses it.
      return standInAgents
        ? route.continue({ postData: JSON.stringify({ ...body, hosted: false }) })
        : route.fulfill({ status: 503, headers: runCors, json: { reason: 'launcher_not_registered', message: 'no launcher named main is registered' } })
    }
    return route.continue()
  })

  // Fills the form in the home dialog for hosted agents and presses Create and run.
  async function createAndRunFromHome(name, researchers) {
    await page.goto(`${UI}/platform-experiments`)
    await page.getByRole('button', { name: 'Advanced', exact: true }).click()
    const dialog = page.getByRole('dialog')
    await dialog.getByPlaceholder('e.g. Q3 Vision Benchmark').fill(name)
    await choose(page, dialog.getByLabel('Common metric'), 'Val Accuracy (↑ max)')
    await dialog.getByRole('button', { name: 'Add', exact: true }).click()
    await dialog.getByRole('button', { name: 'Next: competition settings' }).click()
    await dialog.getByRole('button', { name: 'Hosted agents' }).click()
    assert.ok(await dialog.getByRole('button', { name: 'Create and run' }).isEnabled(), 'hosted agents need no repository')
    await dialog.getByRole('tab', { name: '1. Essentials' }).click()
    await dialog.getByLabel('Repository').click()
    await page.getByRole('option', { name: 'acme/widgets' }).click()
    await dialog.getByRole('tab', { name: '2. Competition settings' }).click()
    await dialog.getByLabel('Researchers').fill(String(researchers))
    runWrites.length = 0
    await dialog.getByRole('button', { name: 'Create and run' }).click()
    await page.waitForURL(u => /^\/platform-experiments\/[^/]+$/.test(u.pathname))
    await page.getByRole('heading', { name }).waitFor()
    return new URL(page.url()).pathname.split('/').pop()
  }

  const ranID = await createAndRunFromHome(`ui-e2e-run-${run}`, 2)
  await page.getByText('The experiment is running:').waitFor()
  assert.deepEqual(runWrites.map(w => w.path), [
    '/platform-experiments', `/platform-experiments/${ranID}/agents`, `/platform-experiments/${ranID}/agents`, `/platform-experiments/${ranID}/start`,
  ], 'create, then one call an agent, then the start')
  assert.equal(runWrites[0].body.repository, 'acme/widgets')
  assert.equal(runWrites[0].body.coordinator_hosted, true)
  assert.ok(!('starts_at' in runWrites[0].body), 'no start time is sent, so nothing starts it between two agents')
  assert.deepEqual(runWrites.slice(1, 3).map(w => w.body), [{ hosted: true }, { hosted: true }], 'each agent is asked for hosted, with no count')
  const ranToken = await tokenOf(ada)
  assert.equal((await (await api(ranToken, `/platform-experiments/${ranID}`)).json()).status, 'running', 'the API says it is running')
  assert.equal((await (await api(ranToken, `/platform-experiments/${ranID}/agents`)).json()).agents.length, 2, 'and holds the two agents')
  assert.equal(await page.locator('.error-banner').count(), 0)
  await shot(page, 'home-create-and-run')

  section('home: a run the API stops part way opens the experiment and says which step failed')
  // The first hosted agent is refused, as by a deployment with no launcher registered.
  standInAgents = false
  const stoppedID = await createAndRunFromHome(`ui-e2e-stopped-${run}`, 2)
  const stoppedBanner = page.locator('.error-banner', { hasText: 'it is not running as asked: adding hosted agent 1 of 2 failed.' })
  await stoppedBanner.getByText('no launcher named main is registered').waitFor()
  assert.deepEqual(runWrites.map(w => w.path), ['/platform-experiments', `/platform-experiments/${stoppedID}/agents`], 'nothing is called after the refusal, and nothing again')
  const stoppedExperiment = await (await api(await tokenOf(ada), `/platform-experiments/${stoppedID}`)).json()
  assert.equal(stoppedExperiment.status, 'open', 'the experiment exists and was not started')
  await page.getByRole('button', { name: 'Start now' }).waitFor()
  await shot(page, 'home-run-stopped', stoppedBanner)
  await stoppedBanner.getByRole('button', { name: 'Dismiss' }).click()
  await stoppedBanner.waitFor({ state: 'detached' })
  await page.unroute(runWrite)
  await page.unroute(runRepositories)
  await page.unroute(runInstallations)

  section('home: one sentence makes a draft, and its proposal is accepted at the cost the person states')
  // A real draft has the platform rent a machine for its planner, so the API's side of a draft
  // is stood in for here: what the page sends is asserted as sent. The API's own behaviour is
  // tests/e2e/test_experiment_draft.py.
  const draftID = `pe-draft-${run}`
  const draftProposal = {
    experiment: {
      name: 'Cosine or constant', description: 'Train the small transformer in train.py and report val_loss.',
      metrics: [{ key: 'val_loss', direction: 'minimize' }], report_interval_seconds: 30,
    },
    budget_accelerator_hours: 6, duration_hours: 4, agents: [{ flavor: 'hyperparameter-search' }, {}],
    baseline_job_id: 'job-baseline', accelerator_type: 'rtx-4090',
    metric_reason: 'Validation loss on held-out text is the goal itself.',
    measurement: 'bench.py scores a split the training never reads.',
    questions: ['Is four hours enough?'],
  }
  const draftGoal = 'Does a cosine learning-rate schedule beat a constant one?'
  let draftNow = {
    id: draftID, project_id: adaMe.projects[0].id, name: draftGoal, goal: draftGoal, status: 'draft', shared: false,
    budget_accelerator_hours: 2, max_agents: 1, signup_count: 1, stages: [{ length_pct: 100, evict_pct: 0 }], current_stage: 1,
    ends_at: new Date(Date.now() + 30 * 60_000).toISOString(), created_at: new Date().toISOString(),
  }
  const draftWrites = []
  const draftAPI = url => url.href === `${API}/platform-experiments/drafts` || url.pathname.startsWith(`/platform-experiments/${draftID}`) || url.pathname === '/experiments/job-baseline/metrics'
  await page.route(draftAPI, route => {
    const request = route.request()
    const path = new URL(request.url()).pathname
    const json = body => route.fulfill({ headers: runCors, json: body })
    if (request.method() === 'OPTIONS') return route.fallback()
    if (request.method() === 'POST') {
      draftWrites.push({ path, body: request.postDataJSON() })
      if (path.endsWith('/revise')) draftNow = { ...draftNow, proposed_at: undefined, revisions: [{ message: request.postDataJSON().message, by: { kind: 'user', id: adaMe.user_id, name: ada.name }, at: new Date().toISOString() }] }
      return json(draftNow)
    }
    if (path === `/platform-experiments/${draftID}`) return json(draftNow)
    if (path.endsWith('/agents')) return json({ agents: [{ platform_experiment_id: draftID, agent_id: 'ag-planner', name: 'Planner', role: 'baseline', hosted: true, flavor: 'planner', signed_up_at: new Date().toISOString(), instance: { state: 'starting', restarts: 0, stopped: false } }] })
    if (path.endsWith('/metrics')) return json([{ experiment_id: 'job-baseline', fraction_complete: 1, metric_name: 'val_loss', metric_value: 1.8, metric_basis: 'raw' }])
    return json([])
  })
  await page.goto(`${UI}/platform-experiments`)
  await page.getByLabel('What do you want to find out?').fill(draftGoal)
  await page.getByRole('button', { name: 'Plan this experiment' }).click()
  await page.waitForURL(u => u.pathname === `/platform-experiments/${draftID}`)
  assert.deepEqual(draftWrites, [{ path: '/platform-experiments/drafts', body: { project_id: adaMe.projects[0].id, goal: draftGoal } }], 'the sentence and the project, and nothing else')
  await page.getByRole('status').filter({ hasText: 'The planner is working out how to measure this' }).waitFor()
  assert.equal(await page.getByRole('button', { name: 'Accept and run' }).count(), 0, 'nothing to accept while it is planned')

  // The planner proposed: the card shows the baseline the job reported, and the questions first.
  draftNow = { ...draftNow, proposal: draftProposal, proposed_at: new Date().toISOString() }
  await page.reload()
  await page.getByText('Proposed experiment: Cosine or constant').waitFor()
  await page.getByText('val_loss, lower is better, baseline 1.8').waitFor()
  await page.getByText('Is four hours enough?').waitFor()
  draftWrites.length = 0
  await page.getByLabel('What should be different?').fill('It must fit one RTX 4090.')
  await page.getByRole('button', { name: 'Revise', exact: true }).click()
  await page.getByRole('status').filter({ hasText: 'The planner is changing its proposal' }).waitFor()
  await page.getByText('It must fit one RTX 4090.').waitFor()
  assert.deepEqual(draftWrites, [{ path: `/platform-experiments/${draftID}/revise`, body: { message: 'It must fit one RTX 4090.' } }])

  draftNow = { ...draftNow, proposed_at: new Date().toISOString() }
  await page.reload()
  draftWrites.length = 0
  await page.getByLabel('Budget (accelerator-hours)').fill('3')
  const draftStarted = page.waitForResponse(response => response.url().endsWith(`/platform-experiments/${draftID}/start`) && response.request().method() === 'POST')
  await page.getByRole('button', { name: 'Accept and run' }).click()
  await draftStarted
  assert.deepEqual(draftWrites, [
    { path: `/platform-experiments/${draftID}/accept`, body: { budget_accelerator_hours: 3, duration_hours: 4, agents: [{ flavor: 'hyperparameter-search' }, {}] } },
    { path: `/platform-experiments/${draftID}/start`, body: null },
  ], 'the budget the person typed, the proposal\'s duration and researchers, then the start')
  await page.unroute(draftAPI)

  section('the idea form has no name field and the idea is filed under the signed-in name')
  await page.goto(`${UI}/hypotheses?pe=${experimentID}`)
  await page.getByRole('button', { name: '+ Add idea under the objective' }).click()
  await page.getByText('registered under the name you signed in with').waitFor()
  assert.equal(await page.getByPlaceholder(/your name/i).count(), 0)
  const ideaText = `a warmup of 500 steps steadies the loss ${run}`
  const ideaWrite = page.waitForResponse(r => r.request().method() === 'POST' && r.url() === `${API}/hypotheses`)
  await page.getByPlaceholder(/the retry loop is masking/).fill(ideaText)
  await page.getByRole('button', { name: 'Add idea', exact: true }).click()
  const idea = await (await ideaWrite).json()
  assert.deepEqual(idea.actor, { kind: 'user', id: adaMe.user_id, name: ada.name }, 'the idea is the signed-in person\'s, by the session alone')
  assert.ok(!('author' in (await ideaWrite).request().postDataJSON()), 'and the request names nobody')
  // The new idea is selected: its panel names who registered it and marks them a person.
  const ideaPanel = page.locator('main').filter({ hasText: ideaText })
  await ideaPanel.getByText(ada.name).first().waitFor()
  await ideaPanel.getByText('human', { exact: true }).first().waitFor()
  await shot(page, 'idea-by-signed-in-name')

  section('a comment is shown under the signed-in name')
  const ideaPage = `${UI}/hypotheses/${idea.id}?project=${adaMe.projects[0].id}`
  await page.goto(ideaPage)
  await page.getByText('Registered by').getByText(ada.name).waitFor()
  const noteText = `try it at batch size 512 first ${run}`
  await page.getByPlaceholder(/A note on this claim/).fill(noteText)
  await page.getByRole('button', { name: 'Add comment' }).click()
  const note = page.locator('div', { hasText: noteText }).filter({ hasText: ada.name }).last()
  await note.getByText('human', { exact: true }).waitFor()
  await note.getByText(ada.name).waitFor()
  await shot(page, 'comment-by-signed-in-name')

  section('settings: a second project, then switching projects shows only what each holds')
  await page.goto(`${UI}/settings`)
  await createInNav(page, 'project', 'Default project')
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
  assert.equal(await page.locator('.wa-nav-tenant').getByRole('combobox').nth(1).inputValue(), 'Default project', 'the switcher names the record\'s project')
  await page.goto(`${UI}/settings?project=${secondProjectID}`)
  await page.getByLabel('Name of project second').waitFor()
  await page.getByRole('link', { name: 'Experiments', exact: true }).click()
  await page.waitForURL(/\/platform-experiments\?project=/)
  await page.getByText(/New experiment in .+ \/ second$/).waitFor()
  await noneOfItsOwn(page).waitFor()
  assert.equal(await page.getByText(experimentName).count(), 0, 'the other project\'s experiment is not listed')
  await shot(page, 'switched-project-is-empty')
  await selectInNav(page, 'Project', 'Default project')
  await page.getByText(experimentName).first().waitFor()
  await shot(page, 'switched-back-shows-experiment')

  section('settings: renaming the project and the org')
  await page.goto(`${UI}/settings`)
  await page.getByLabel('Name of project second').fill('Default project')
  await page.getByLabel('Name of project second').locator('..').getByRole('button', { name: 'Rename' }).click()
  await page.getByLabel('Name of project second').locator('..').getByText('already').waitFor()
  await shot(page, 'project-rename-collision', page.getByLabel('Name of project second'))
  await page.getByLabel('Name of project second').fill('renamed')
  await page.getByLabel('Name of project second').locator('..').getByRole('button', { name: 'Rename' }).click()
  await page.getByLabel('Name of project renamed').waitFor()
  await page.getByLabel('New project name').last().fill('Default project')
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
  await confirmWith(page, 'Revoke')
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
    new RegExp(`^sh -c '[^']*helm upgrade --install hypothesisloop-cluster-agent "\\$chart" --version "\\$version" --namespace hypothesisloop --create-namespace --take-ownership --set-file runtimeToken=/dev/stdin "\\$@"' sh 'oci://[^']+/charts/hypothesisloop-cluster-agent' '0\\.1\\.0\\+[^']+' --set-string 'clusterName=${clusterName}' --set-string 'apiURL=https?://[^']+' --set-string 'metricsURL=https?://[^']+' --set-string 'images\\.registry=[^']+' --set-string 'images\\.tag=[^']+' --set-string 'jobImage=[^']+'$`),
    'the command is the one hl cluster add prints, with the addresses the platform answered with',
  )
  const registered = await (await api(await tokenOf(ada), `/orgs/${adaMe.orgs[0].id}/clusters`, 'POST', { name: clusterName })).json()
  const runtimeToken = registered.runtime_token
  assert.match(runtimeToken, /^hls_/)
  secrets.add(runtimeToken)
  assert.ok(!command.includes(runtimeToken) && !command.includes('hl cluster token'), 'the command holds no token and needs no hl')
  assert.equal(await page.getByTestId('runtime-token').innerText(), runtimeToken, 'the token the command asks for is shown beside it')
  assert.ok(command.includes(`--set-string 'apiURL=${registered.api_url}' --set-string 'metricsURL=${registered.metrics_url}' --set-string 'images.registry=${registered.registry}' --set-string 'images.tag=${registered.image_tag}' --set-string 'jobImage=${registered.job_image}'`))
  await page.getByRole('group', { name: 'Kind of cluster' }).getByRole('button', { name: 'Bare-metal node' }).click()
  const nodeCommand = await page.getByTestId('secret').innerText()
  assert.ok(
    nodeCommand.startsWith(`sudo sh -c 'engine="$(command -v docker || command -v podman)" || `) &&
      nodeCommand.endsWith(` install --cluster "$1" --api-url "$2" --job-image "$4"' sh '${clusterName}' '${registered.api_url}' '${registered.registry}/hypothesisloop-bare-agent:${registered.image_tag}' '${registered.job_image}'`),
    `a bare-metal node is shown the command that installs its agent: ${nodeCommand}`,
  )
  assert.ok(!nodeCommand.includes(runtimeToken) && !nodeCommand.includes('hl cluster token'), 'the node command holds no token and needs no hl')
  assert.equal(await page.getByTestId('runtime-token').innerText(), runtimeToken, 'the token the node command asks for is shown beside it')
  await shot(page, 'cluster-added-bare-metal', page.getByTestId('secret'))
  await page.getByRole('group', { name: 'Kind of cluster' }).getByRole('button', { name: 'Kubernetes cluster' }).click()
  await page.getByRole('cell', { name: clusterName }).first().waitFor()
  await shot(page, 'cluster-added', page.getByRole('cell', { name: clusterName }).first())
  await page.getByRole('button', { name: 'Hide' }).click()
  await page.getByRole('button', { name: 'Show start command' }).click()
  assert.equal(await page.getByTestId('secret').innerText(), command, 'asking again shows the same command')
  await page.getByRole('button', { name: 'Hide' }).click()
  // A second cluster of the org's own: the two are a list in the order jobs are placed on them,
  // and moving one up is written to the API as priorities.
  const secondCluster = `ui-e2e-second-${run}`
  const clustersAPI = `/orgs/${adaMe.orgs[0].id}/clusters`
  assert.ok((await api(await tokenOf(ada), clustersAPI, 'POST', { name: secondCluster })).ok, 'a second cluster is registered')
  await page.reload()
  await page.getByRole('button', { name: `Move ${secondCluster} up` }).click()
  await page.getByRole('button', { name: `Move ${clusterName} up` }).and(page.locator(':enabled')).waitFor()
  const ordered = (await (await api(await tokenOf(ada), clustersAPI)).json()).filter(c => c.owned && !c.shared).map(c => c.name)
  assert.deepEqual(ordered.slice(0, 2), [secondCluster, clusterName], 'the API lists the moved cluster first')
  assert.ok(!(await page.content()).includes(runtimeToken), 'hiding the command takes the runtime token off the page')

  section('the settings editor on an owned cluster, once its agent has reported')
  const clusterID = `id-${clusterName}`
  const reported = await api(runtimeToken, `/internal/clusters/${clusterName}/reconcile`, 'POST', idleReport(clusterID))
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
  await page.getByRole('cell', { name: new RegExp(clusterName) }).first().waitFor()
  assert.equal(await page.getByRole('button', { name: 'Show start command' }).count(), 0, 'the cluster page manages nothing')
  assert.equal(await page.getByText('staff only').count(), 0, 'no staff view for a customer')
  await shot(page, 'cluster-page')
  await page.getByRole('link', { name: 'Statistics' }).click()
  await page.getByText('Job Status Breakdown').waitFor()
  assert.equal(await page.getByText('staff only').count(), 0)

  section('github: what the real API says of an org with no account connected')
  const orgID = adaMe.orgs[0].id
  const settingsPage = `${UI}/settings?project=${secondProjectID}`
  const installationsAPI = `${API}/orgs/${orgID}/github-installations`
  const appRead = page.waitForResponse(`${API}/github/app`)
  const installationsRead = page.waitForResponse(r => r.url().startsWith(installationsAPI) && r.request().method() === 'GET')
  await page.goto(settingsPage)
  const githubCard = page.locator('#github')
  assert.equal((await installationsRead).status(), 200)
  assert.deepEqual(await (await installationsRead).json(), [], 'a new org has connected no GitHub account')
  await githubCard.getByText(`No GitHub account is connected to ${ada.name} Lab, so its platform experiments run without a repository.`).waitFor()
  const stackHasGitHub = (await appRead).status() === 200
  if (stackHasGitHub) {
    await githubCard.getByRole('button', { name: 'Connect GitHub' }).waitFor()
  } else {
    assert.equal((await (await appRead).json()).reason, 'github_not_configured')
    await githubCard.getByText('This deployment has no GitHub App configured, so no GitHub account can be connected.').waitFor()
    assert.equal(await githubCard.getByRole('button', { name: 'Connect GitHub' }).count(), 0)
    await shot(page, 'github-not-configured', githubCard)
  }

  section('github: connecting leaves for GitHub once and returns to the card, with the real API\'s answer')
  await page.route(`${API}/github/app`, route => route.fulfill({
    headers: { 'access-control-allow-origin': UI },
    json: { slug: 'hl-e2e', client_id: 'Iv1.e2e', install_url: `${IDP}/github/install`, authorize_url: `${IDP}/github/authorize?client_id=Iv1.e2e` },
  }))
  const connects = []
  const returns = []
  const authorizes = []
  page.on('request', request => {
    const url = new URL(request.url())
    if (request.url().startsWith(installationsAPI) && request.method() === 'POST') connects.push(request)
    if (request.url().startsWith(`${UI}/settings/repository`) && (url.searchParams.has('code') || url.searchParams.has('error'))) returns.push(request.url())
    if (request.url().startsWith(`${IDP}/github/authorize`)) authorizes.push(url)
  })
  const landed = `${UI}/settings?project=${secondProjectID}#github`
  await page.reload()
  await githubCard.getByRole('button', { name: 'Connect GitHub' }).waitFor()
  await shot(page, 'github-none-connected', githubCard)
  await githubCard.getByRole('button', { name: 'Connect GitHub' }).dblclick()
  await page.waitForURL(landed)
  assert.equal(authorizes.length, 1, 'a double click left for GitHub once')
  assert.equal(authorizes[0].searchParams.get('redirect_uri'), `${UI}/settings/repository`, 'the way back is this UI\'s own origin, at the address the App has registered')
  assert.equal(new URL(returns[0]).searchParams.get('state'), authorizes[0].searchParams.get('state'))
  assert.equal(connects.length, 1)
  const githubCode = (await idpState()).githubCodes.at(-1)
  assert.deepEqual(connects[0].postDataJSON(), { code: githubCode, redirect_uri: `${UI}/settings/repository` }, 'no installation is named: the API finds the one the person reaches')
  // No GitHub knows this code: a stack without an App answers 503, one with an App is refused the code.
  const refusedConnect = await connects[0].response()
  assert.equal(refusedConnect.status(), stackHasGitHub ? 422 : 503)
  assert.equal((await refusedConnect.json()).reason, stackHasGitHub ? 'code_rejected' : 'github_not_configured')
  await githubCard.locator('.error-banner', { hasText: 'GitHub was not connected:' }).waitFor()
  await shot(page, 'github-connect-refused', githubCard)

  section('github: a replayed return, a forged one and Back do nothing')
  await page.goto(returns[0])
  await page.waitForURL(`${UI}/settings?project=${adaMe.projects[0].id}#github`)
  await githubCard.locator('.error-banner', { hasText: 'This return from GitHub does not belong to a connect started in this tab' }).waitFor()
  await shot(page, 'github-return-refused', githubCard)
  await page.goto(`${UI}/settings/repository?project=${secondProjectID}&code=stolen&state=forged`)
  await page.waitForURL(landed)
  await githubCard.locator('.error-banner', { hasText: 'This return from GitHub does not belong to a connect started in this tab' }).waitFor()
  assert.equal(connects.length, 1, 'neither return was sent to the API')
  // Leaving for GitHub replaces the page, so Back from the return is never GitHub or a return with a code in it.
  await page.goBack()
  assert.ok(!new URL(page.url()).searchParams.has('code'), 'Back does not lead to a return')
  assert.equal(authorizes.length, 1, 'and asked GitHub nothing')
  await page.goto(settingsPage)

  section('github: GitHub declined')
  await configure({ githubDeclines: true })
  await githubCard.getByRole('button', { name: 'Connect GitHub' }).click()
  await page.waitForURL(landed)
  await githubCard.locator('.error-banner', { hasText: 'GitHub did not authorise the App: The user has denied your application access.' }).waitFor()
  assert.equal(connects.length, 1, 'a refusal by GitHub is not sent to the API')
  await shot(page, 'github-declined', githubCard)
  await configure({ githubDeclines: false })

  section('github: the App installed nowhere, several accounts to choose from, then connected, listed and disconnected')
  // The API is stood in for from here: no real GitHub account stands behind the stand-in's codes.
  const cors = { 'access-control-allow-origin': UI, 'access-control-expose-headers': 'X-Total-Count' }
  let connected = []
  let connectAnswer = { status: 422, json: { reason: 'app_not_installed', message: 'the GitHub account reaches no installation of the App' } }
  await page.route(url => url.href.startsWith(installationsAPI), route => {
    const request = route.request()
    if (request.method() === 'POST') {
      if (connectAnswer.status === 201) connected = [connectAnswer.json]
      return route.fulfill({ status: connectAnswer.status, headers: cors, json: connectAnswer.json })
    }
    if (request.method() === 'DELETE') {
      connected = []
      return route.fulfill({ status: 204, headers: cors })
    }
    if (request.method() === 'GET') return route.fulfill({ headers: { ...cors, 'X-Total-Count': String(connected.length) }, json: connected })
    return route.fallback()
  })
  await githubCard.getByRole('button', { name: 'Connect GitHub' }).click()
  await page.waitForURL(landed)
  await githubCard.locator('.error-banner', { hasText: 'Your GitHub account reaches no installation of the App.' }).waitFor()
  await shot(page, 'github-app-not-installed', githubCard)

  // Installing the App returns with the installation it made and no state: the tab that left to
  // install goes on to authorise, and the connect names that installation.
  connectAnswer = {
    status: 201,
    json: { org_id: orgID, installation_id: 4242, account_login: 'acme', connected_by: adaMe.user_id, connected_by_name: ada.name, connected_at: new Date().toISOString() },
  }
  const authorizesBeforeInstall = authorizes.length
  await githubCard.getByRole('button', { name: 'Install the App on an account' }).click()
  await page.waitForURL(landed)
  assert.equal(authorizes.length, authorizesBeforeInstall + 1, 'the return from installing went on to authorise, once')
  assert.equal(connects.at(-1).postDataJSON().installation_id, 4242, 'for the installation GitHub returned')
  await githubCard.getByText('Connected acme.').waitFor()
  const accountRow = githubCard.getByRole('row', { name: /acme/ })
  await accountRow.getByText(ada.name).waitFor()
  await shot(page, 'github-connected', githubCard)

  await accountRow.getByRole('button', { name: 'Disconnect' }).click()
  await page.getByRole('dialog').getByText(`Disconnect acme from ${ada.name} Lab?`).waitFor()
  await page.getByRole('dialog').getByRole('button', { name: 'Cancel' }).click()
  await accountRow.waitFor()
  await accountRow.getByRole('button', { name: 'Disconnect' }).click()
  await confirmWith(page, 'Disconnect')
  await githubCard.getByText('No GitHub account is connected to').waitFor()

  connectAnswer = {
    status: 409,
    json: {
      reason: 'installation_choice_required', message: 'the GitHub account reaches several installations: name one',
      installations: [{ installation_id: 4242, account_login: 'acme' }, { installation_id: 4343, account_login: 'ada-personal' }],
    },
  }
  await githubCard.getByRole('button', { name: 'Connect GitHub' }).click()
  await page.waitForURL(landed)
  const choice = githubCard.getByRole('group', { name: 'GitHub account to connect' })
  await choice.getByRole('button', { name: 'acme', exact: true }).waitFor()
  await shot(page, 'github-choose-account', githubCard)
  connectAnswer = {
    status: 201,
    json: { org_id: orgID, installation_id: 4343, account_login: 'ada-personal', connected_by: adaMe.user_id, connected_by_name: ada.name, connected_at: new Date().toISOString() },
  }
  await choice.getByRole('button', { name: 'ada-personal' }).click()
  await page.waitForURL(landed)
  assert.equal(connects.at(-1).postDataJSON().installation_id, 4343, 'the second authorisation connects the account that was chosen')
  await githubCard.getByText('Connected ada-personal.').waitFor()

  section('creating an experiment offers the repositories of the connected accounts, and the agent image under Advanced')
  const repositoriesAPI = `${API}/orgs/${orgID}/github-repositories`
  const searches = []
  await page.route(url => url.href.startsWith(repositoriesAPI), route => {
    const q = new URL(route.request().url()).searchParams.get('q') ?? ''
    searches.push(q)
    const found = [
      { repository: 'ada-personal/widgets', installation_id: 4343, private: true },
      { repository: 'ada-personal/open', installation_id: 4343, private: false },
    ].filter(r => r.repository.includes(q))
    return route.fulfill({ headers: { ...cors, 'X-Total-Count': String(found.length) }, json: found })
  })
  await page.getByRole('link', { name: 'Experiments', exact: true }).click()
  await page.getByRole('button', { name: 'Advanced', exact: true }).click()
  const repositoryField = page.getByLabel('Repository')
  await repositoryField.click()
  await page.getByRole('option', { name: 'No repository' }).waitFor()
  assert.equal(await page.getByRole('option', { name: /ada-personal\/widgets/ }).innerText().then(t => t.replace(/\s+/g, ' ')), 'ada-personal/widgets private')
  await repositoryField.fill('wid')
  await page.getByRole('option', { name: /ada-personal\/open/ }).waitFor({ state: 'detached' })
  assert.ok(searches.includes('wid') && !searches.includes('w'), 'the search waited for typing to pause')
  await page.getByRole('option', { name: /ada-personal\/widgets/ }).click()
  assert.equal(await repositoryField.inputValue(), 'ada-personal/widgets')
  assert.equal(await page.getByLabel('Agent image').count(), 0, 'the agent image is folded away')
  await page.getByRole('button', { name: 'Advanced' }).click()
  await page.getByLabel('Agent image').fill('ghcr.io/ada/agent:1')
  await shot(page, 'create-experiment-repository', page.getByLabel('Agent image'))
  await page.keyboard.press('Escape')
  await page.unroute(url => url.href.startsWith(repositoriesAPI))
  await page.unroute(url => url.href.startsWith(installationsAPI))
  await page.unroute(`${API}/github/app`)

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
  assert.equal(await page.locator('.wa-nav-tenant').getByRole('combobox').nth(1).inputValue(), 'Default project')
  await page.getByText(`New experiment in ${ada.name} Second / Default project`).waitFor()
  await noneOfItsOwn(page).waitFor()
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
  await page.getByRole('cell', { name: new RegExp(clusterName) }).first().waitFor()

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
  await page.getByText(`${ada.name} Second: its name, projects, members, API keys, clusters and GitHub accounts.`).waitFor()
  assert.equal(await page.getByTestId('secret').count(), 0, 'the other org\'s key and start command are gone')
  assert.ok(!(await page.content()).includes(shownKey), 'the other org\'s API key is nowhere on the page')
  assert.equal(await page.getByLabel('Cluster name').inputValue(), '', 'the half-typed cluster name is gone')
  await shot(page, 'token-gone-after-switch')
  await selectInNav(page, 'Organization', `${ada.name} Lab`)
  await page.getByRole('cell', { name: new RegExp(clusterName) }).first().waitFor()

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
  await noneOfItsOwn(otherPage).waitFor()
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

  section('shared: a platform experiment staff shared is in another account\'s own list, marked and read only')
  const sharedName = `${experimentName}-edited`
  const bobToken = await tokenOf(bob)
  const bobMe = await (await api(bobToken, '/me')).json()
  const bobProjects = bobMe.projects.map(p => p.id)
  const experimentsList = r => r.url().startsWith(`${API}/platform-experiments?`) && r.request().method() === 'GET' && new URL(r.url()).searchParams.get('limit') === '10'
  share(experimentID)
  try {
    // Everything the pages ask the API from here on, to check what they ask and what it answers.
    const sharedCalls = []
    const record = response => {
      if (response.url().startsWith(API) && response.request().method() !== 'OPTIONS') sharedCalls.push(response)
    }
    otherPage.on('response', record)

    const listed = otherPage.waitForResponse(experimentsList)
    await otherPage.getByRole('link', { name: 'Experiments', exact: true }).click()
    const list = await listed
    const asked = new URL(list.url()).searchParams
    assert.ok(bobProjects.includes(asked.get('project_id')), 'the list is the one of the reader\'s own project')
    assert.equal(asked.get('shared'), null, 'with no filter on shared')
    const listedShared = (await list.json()).find(pe => pe.id === experimentID)
    assert.equal(listedShared?.shared, true, 'the API lists the shared experiment in another org\'s project')
    assert.ok(Number(list.headers()['x-total-count']) >= 1, 'and counts it')
    const card = otherPage.locator('[role="link"]', { hasText: sharedName })
    await card.getByText('Shared · read only').waitFor()
    assert.equal(await card.getByRole('button', { name: 'Edit' }).count(), 0, 'a shared experiment of another org offers no edit')
    await otherPage.getByRole('button', { name: 'Advanced', exact: true }).waitFor()
    await shot(otherPage, 'shared-in-the-list', card)

    section('shared: it opens on the experiment page from the reader\'s own project, with nothing to change')
    await card.click()
    await otherPage.waitForURL(u => u.pathname === `/platform-experiments/${experimentID}`)
    await otherPage.getByRole('heading', { name: sharedName }).waitFor()
    await otherPage.getByRole('note').filter({ hasText: 'shares this experiment with every account' }).waitFor()
    await otherPage.getByText('Shared · read only').waitFor()
    assert.equal(await otherPage.getByRole('note').count(), 1, 'one note says it is shared and read only')
    assert.equal(await otherPage.locator('.wa-nav-tenant').getByRole('combobox').first().inputValue(), 'Default org', 'the switcher still names the reader\'s own org')
    // The owner's page shows Add agent, and Show token and Remove in the row of its one agent.
    const sharedAgents = otherPage.locator('table', { hasText: 'Run by' }).locator('tbody tr')
    await sharedAgents.first().waitFor()
    assert.equal(await sharedAgents.count(), 1)
    for (const control of ['Add agent', 'Show token', 'Remove', 'Edit']) {
      assert.equal(await otherPage.getByRole('button', { name: control }).count(), 0, `a shared experiment of another org shows no ${control}`)
    }
    assert.equal(await otherPage.locator('.error-banner').count(), 0, 'every read of the page is answered')
    assert.ok([null, ...bobProjects].includes(new URL(otherPage.url()).searchParams.get('project')), 'the URL names no project of the other org')
    await shot(otherPage, 'shared-read-only')

    await otherPage.getByRole('link', { name: /Hypotheses →/ }).click()
    await otherPage.waitForURL(u => u.pathname === '/hypotheses' && u.searchParams.get('pe') === experimentID)
    await otherPage.getByRole('link', { name: 'Experiment →' }).waitFor()
    assert.equal(await otherPage.getByRole('button', { name: /Add idea/ }).count(), 0, 'its graph takes no idea')
    await shot(otherPage, 'shared-hypotheses')
    await otherPage.getByRole('link', { name: 'Experiment →' }).click()
    await otherPage.waitForURL(u => u.pathname === `/platform-experiments/${experimentID}`)

    await otherPage.getByRole('link', { name: /^Jobs/ }).last().click()
    await otherPage.waitForURL(u => u.pathname === '/jobs' && u.searchParams.get('platform_experiment_id') === experimentID)
    await otherPage.getByText('No jobs found').waitFor()
    assert.equal(await otherPage.getByRole('button', { name: 'Cancel' }).count(), 0)
    await shot(otherPage, 'shared-jobs')

    // What the pages asked: the lists of what is in the shared experiment by its id, never
    // within a project of the reader's, nothing but reads, and nothing the API refused.
    const inShared = sharedCalls.filter(c => /^\/(experiments|agents|donations)(\/stats)?$/.test(new URL(c.url()).pathname)
      && new URL(c.url()).searchParams.get('platform_experiment_id') === experimentID)
    assert.ok(inShared.length > 3, 'the pages read the lists of the shared experiment')
    assert.ok(inShared.some(c => new URL(c.url()).searchParams.get('project_id') === null), 'its jobs are asked for with no project')
    for (const call of inShared) {
      assert.ok(!bobProjects.includes(new URL(call.url()).searchParams.get('project_id')), `${call.url()} names no project of the reader`)
    }
    for (const call of sharedCalls) {
      const { pathname } = new URL(call.url())
      assert.ok(call.request().method() === 'GET' || pathname === '/hypotheses/query', `${call.request().method()} ${call.url()} only reads`)
      assert.ok(call.status() < 400, `${call.url()} is answered ${call.status()}`)
    }
    otherPage.off('response', record)

    section('shared: a link its owner copied opens for the reader, the owner\'s project dropped from it')
    await otherPage.goto(`${UI}/platform-experiments/${experimentID}?project=${adaMe.projects[0].id}`)
    await otherPage.waitForURL(u => u.pathname === `/platform-experiments/${experimentID}` && u.searchParams.get('project') !== adaMe.projects[0].id)
    await otherPage.getByRole('heading', { name: sharedName }).waitFor()
    assert.equal(await otherPage.getByText('is not available to you').count(), 0)
    assert.equal(await otherPage.getByRole('button', { name: 'Add agent' }).count(), 0)

    section('shared: the API refuses the reader every write to it')
    assert.equal((await api(bobToken, `/platform-experiments/${experimentID}/agents`, 'POST', { hosted: false })).status, 404)
    assert.equal((await api(bobToken, `/platform-experiments/${experimentID}/close`, 'POST')).status, 404)

    section('shared: it does not count as the reader\'s own, and the pickers offer it')
    await otherPage.goto(`${UI}/platform-experiments`)
    await otherPage.getByText('New experiment in').waitFor()
    await otherPage.getByRole('heading', { name: 'Experiments', exact: true }).waitFor()
    await otherPage.goto(`${UI}/jobs`)
    await otherPage.getByRole('heading', { name: 'Jobs' }).waitFor()
    await otherPage.getByPlaceholder('All experiments').click()
    await otherPage.getByRole('option', { name: new RegExp(`${sharedName} \\(\\w+, shared\\)`) }).click()
    await otherPage.waitForURL(u => u.searchParams.get('platform_experiment_id') === experimentID)
    await otherPage.getByText('No jobs found').waitFor()

    section('shared: its own org sees it marked and changes it as before')
    await page.goto(`${UI}/platform-experiments?project=${adaMe.projects[0].id}`)
    const ownCard = page.locator('[role="link"]', { hasText: sharedName })
    await ownCard.getByText('Shared', { exact: true }).waitFor()
    await ownCard.getByRole('button', { name: 'Edit' }).waitFor()
    await ownCard.click()
    await page.waitForURL(u => u.pathname === `/platform-experiments/${experimentID}`)
    await page.getByRole('button', { name: 'Add agent' }).waitFor()
    assert.equal(await page.getByRole('note').count(), 0, 'its own org is told of no read-only')

    section('shared: staff have no control to share from')
    hlAdmin('user', 'staff', bob.id, 'on')
    try {
      await otherPage.goto(`${UI}/platform-experiments`)
      await otherPage.getByText(sharedName).first().waitFor()
      assert.equal(await otherPage.getByRole('button', { name: /share/i }).count(), 0)
    } finally {
      hlAdmin('user', 'staff', bob.id, 'off')
    }
  } finally {
    unshare(experimentID)
  }

  section('shared: an experiment staff took back is gone from every other org\'s list')
  const relisted = otherPage.waitForResponse(experimentsList)
  await otherPage.goto(`${UI}/platform-experiments`)
  assert.ok(!(await (await relisted).json()).some(pe => pe.id === experimentID), 'the API lists it no more')
  await noneOfItsOwn(otherPage).waitFor()
  assert.equal(await otherPage.getByText(sharedName).count(), 0)
  await otherPage.goto(`${UI}/platform-experiments/${experimentID}`)
  await otherPage.locator('.error-banner', { hasText: 'Could not load this platform experiment' }).waitFor()
  await otherPage.goto(`${UI}/jobs`)
  await otherPage.getByRole('heading', { name: 'Jobs' }).waitFor()

  section('members: the last owner cannot leave, and an invite link is shown once')
  const labOrg = adaMe.orgs[0].id
  const labProject = adaMe.projects[0].id
  const pendingInvites = async () => (await api(await tokenOf(ada), `/orgs/${labOrg}/invites`)).json()
  await page.goto(`${UI}/settings?project=${labProject}`)
  // A member is listed by the name their identity provider states, their id on hover.
  const membersTable = page.getByRole('table', { name: 'Members' })
  const adaRow = membersTable.getByRole('row', { name: new RegExp(ada.name) })
  await adaRow.getByText('you', { exact: true }).waitFor()
  assert.equal(await adaRow.getByText(ada.name).getAttribute('title'), adaMe.user_id)
  assert.equal(await adaRow.getByText(adaMe.user_id).count(), 0, 'the id is not what the row reads')
  await page.getByText('No pending invites.').waitFor()
  await adaRow.getByRole('button', { name: 'Leave' }).click()
  await confirmWith(page, 'Leave')
  await page.locator('.error-banner', { hasText: 'an org keeps at least one owner' }).waitFor()
  await shot(page, 'members-last-owner-refused', adaRow)

  await page.getByRole('button', { name: 'Create invite link' }).click()
  const revokedLink = await page.getByTestId('secret').innerText()
  assert.ok(revokedLink.startsWith(`${UI}/invite#hli_`), 'the link is this site\'s invite page with the token in the fragment')
  secrets.add(revokedLink.split('#')[1])
  await page.getByRole('button', { name: 'Copy', exact: true }).click()
  assert.equal(await page.evaluate(() => navigator.clipboard.readText()), revokedLink)
  await shot(page, 'invite-link-created', page.getByTestId('secret'))
  await page.getByRole('button', { name: 'I have sent it' }).click()
  await page.reload()
  const [revokedInvite] = await pendingInvites()
  const inviteRow = page.getByRole('row', { name: new RegExp(revokedInvite.id) })
  await inviteRow.waitFor()
  await inviteRow.getByText(ada.name).waitFor()
  assert.ok(!(await page.content()).includes(revokedLink.split('#')[1]), 'the token is not on the page after a reload')

  section('members: a revoked invite is refused')
  await inviteRow.getByRole('button', { name: 'Revoke' }).click()
  await confirmWith(page, 'Revoke')
  await page.getByText('No pending invites.').waitFor()
  await otherPage.goto(revokedLink)
  await otherPage.getByRole('button', { name: 'Accept invite' }).click()
  await otherPage.locator('.error-banner', { hasText: 'This invite was revoked.' }).waitFor()
  assert.equal(otherPage.url(), `${UI}/invite`, 'the token is out of the address')
  await shot(otherPage, 'invite-revoked')

  section('members: a signed-out person opens the link, signs in, accepts and lands in the org')
  await page.getByRole('button', { name: 'Create invite link' }).click()
  const inviteAddress = await page.getByTestId('secret').innerText()
  secrets.add(inviteAddress.split('#')[1])
  const invited = await newContext(browser)
  const danPage = await invited.newPage()
  await danPage.goto(inviteAddress)
  await danPage.getByRole('button', { name: 'Sign in to accept' }).click()
  await danPage.waitForURL(`${UI}/login?next=%2Finvite`)
  await signIn(danPage, dan, `${UI}/invite`)
  await shot(danPage, 'invite-offered')
  await danPage.getByRole('button', { name: 'Accept invite' }).click()
  await danPage.waitForURL(`${UI}/platform-experiments?project=${labProject}`)
  const danID = (await (await api(await tokenOf(dan), '/me')).json()).user_id
  assert.match(danID, /^[\w-]+$/)
  assert.deepEqual(await pendingInvites(), [], 'an accepted invite is no longer pending')

  await otherPage.goto(inviteAddress)
  await otherPage.getByRole('button', { name: 'Accept invite' }).click()
  await otherPage.locator('.error-banner', { hasText: 'This invite was already accepted' }).waitFor()
  await shot(otherPage, 'invite-already-used')
  await otherPage.goto(`${UI}/invite`)
  await otherPage.locator('.error-banner', { hasText: 'This page holds no invite.' }).waitFor()

  section('members: a member reads the list, and the owner changes a role and removes them')
  await danPage.goto(`${UI}/settings?project=${labProject}`)
  const danMembers = danPage.getByRole('table', { name: 'Members' })
  const danOwnRow = danMembers.getByRole('row', { name: new RegExp(dan.name) })
  await danOwnRow.getByRole('cell', { name: 'member', exact: true }).waitFor()
  // The one who was invited sees who was there before them, by name.
  await danMembers.getByRole('row', { name: new RegExp(ada.name) }).getByRole('cell', { name: 'owner', exact: true }).waitFor()
  await danPage.goto(ideaPage)
  await danPage.getByText('Registered by').getByText(ada.name).waitFor()
  await danPage.locator('div', { hasText: noteText }).filter({ hasText: ada.name }).last().waitFor()
  await shot(danPage, 'idea-read-by-another-member')
  await danPage.goto(`${UI}/settings?project=${labProject}`)
  await danOwnRow.waitFor()
  await danOwnRow.getByRole('button', { name: 'Leave' }).waitFor()
  assert.equal(await danPage.getByRole('button', { name: 'Create invite link' }).count(), 0)
  assert.equal(await danPage.getByRole('button', { name: 'Remove' }).count(), 0)
  assert.equal(await danPage.getByLabel(/^Role of /).count(), 0)
  await shot(danPage, 'members-as-member', danOwnRow)

  await page.reload()
  const roleChange = () => page.waitForResponse(r => r.request().method() === 'PATCH' && r.url() === `${API}/orgs/${labOrg}/members/${danID}`)
  let changed = roleChange()
  await choose(page, page.getByLabel(`Role of ${dan.name}`), 'owner')
  await confirmWith(page, 'Make owner')
  assert.equal((await changed).status(), 200)
  await shot(page, 'members-as-owner', page.getByLabel(`Role of ${dan.name}`))
  await danPage.reload()
  await danPage.getByRole('button', { name: 'Create invite link' }).waitFor()
  changed = roleChange()
  await choose(page, page.getByLabel(`Role of ${dan.name}`), 'member')
  await confirmWith(page, 'Make member')
  assert.equal((await changed).status(), 200)

  const danRow = membersTable.getByRole('row', { name: new RegExp(dan.name) })
  await danRow.getByRole('button', { name: 'Remove' }).click()
  await confirmWith(page, 'Remove')
  await danRow.waitFor({ state: 'detached' })
  await danPage.reload()
  await danPage.getByText('is not available to you').waitFor()
  await invited.close()

  section('a member of an org sees its work and can change nothing about the org')
  const bobID = (await (await api(await tokenOf(bob), '/me')).json()).user_id
  const labID = adaMe.orgs[0].id
  const bobInvite = await api(await tokenOf(ada), `/orgs/${labID}/invites`, 'POST', { role: 'member' })
  assert.equal(bobInvite.status, 201)
  const bobInviteToken = (await bobInvite.json()).token
  secrets.add(bobInviteToken)
  assert.equal((await api(await tokenOf(bob), '/invites/accept', 'POST', { token: bobInviteToken })).status, 201)
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
    await otherPage.getByRole('link', { name: 'Experiments', exact: true }).click()
    await otherPage.getByText(`${experimentName}-edited`).first().waitFor()

    const appReads = []
    otherPage.on('request', r => { if (r.url() === `${API}/github/app`) appReads.push(r.url()) })
    const memberInstallationsAPI = `${API}/orgs/${labID}/github-installations`
    await otherPage.route(url => url.href.startsWith(memberInstallationsAPI), route => route.request().method() === 'GET'
      ? route.fulfill({
        headers: { 'access-control-allow-origin': UI, 'access-control-expose-headers': 'X-Total-Count', 'X-Total-Count': '1' },
        json: [{ org_id: labID, installation_id: 4242, account_login: 'acme', connected_by: adaMe.user_id, connected_by_name: ada.name, connected_at: new Date().toISOString() }],
      })
      : route.fallback())
    await otherPage.goto(`${UI}/settings?project=${adaMe.projects[0].id}`)
    const memberGitHub = otherPage.locator('#github')
    await memberGitHub.getByRole('cell', { name: 'acme', exact: true }).waitFor()
    assert.equal(await memberGitHub.getByRole('button').count(), 0, 'a member connects and disconnects nothing')
    assert.deepEqual(appReads, [], 'a member\'s page asks nothing about the App')
    await shot(otherPage, 'member-github-accounts', memberGitHub)
    await otherPage.unroute(url => url.href.startsWith(memberInstallationsAPI))
  } finally {
    assert.equal((await api(await tokenOf(ada), `/orgs/${labID}/members/${bobID}`, 'DELETE')).status, 204)
  }

  section('staff see the views across every org, and a customer does not')
  hlAdmin('user', 'staff', bob.id, 'on')
  try {
    await otherPage.goto(`${UI}/cluster`)
    await otherPage.getByText(`Clusters Default org can run on`).waitFor()
    await otherPage.getByText('All Clusters · every org · staff only').waitFor()
    await otherPage.getByRole('columnheader', { name: 'Autoscaler' }).waitFor()
    await shot(otherPage, 'staff-cluster-page', otherPage.getByRole('columnheader', { name: 'Autoscaler' }))
    await otherPage.goto(`${UI}/statistics`)
    await otherPage.getByText('Scheduler Quality · every org · staff only').waitFor()
    await otherPage.getByText('Burst eviction rate').waitFor()
    await shot(otherPage, 'staff-statistics', otherPage.getByText('Burst eviction rate'))
  } finally {
    hlAdmin('user', 'staff', bob.id, 'off')
  }

  section('sign-out ends the session')
  await otherPage.getByRole('button', { name: 'Sign out' }).click()
  await otherPage.waitForURL(`${UI}/`)
  await otherPage.getByRole('link', { name: 'Sign in' }).waitFor()
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

main().then(() => process.exit(), error => {
  console.error(error)
  process.exit(1)
})

// Walks the path of a new person on the hosted stack: sign in, open the full form behind
// Advanced on the home page, pick a repository, Create and run,
// and watch the experiment. It rents real machines, so it is run by hand:
//   node e2e/walk.mjs [--until form|filled|created|done] [--off cluster,cluster]
// It keeps no screenshot and no page text: it prints the steps it passed and what it timed.
// --off switches those clusters off for the person's org in Settings before the form is filled.
// It serves its own copy of the UI signed in through the local identity provider, as run.mjs does.
import assert from 'node:assert/strict'
import { execFileSync, spawn } from 'node:child_process'
import { randomUUID } from 'node:crypto'
import { once } from 'node:events'
import { rmSync } from 'node:fs'
import { dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'
import { chromium } from 'playwright'
import { startIdentityProvider } from './identity-provider.mjs'
import { startProxy } from './proxy.mjs'

const here = dirname(fileURLToPath(import.meta.url))
const repo = join(here, '..', '..', '..')
const UI = 'http://localhost:3000'
const UI_PORT = 3100
const PROXY_PORT = 54398
const UI_BUILD = '.next-walk'
const API = 'http://localhost:8081'
const IDP = 'http://127.0.0.1:54399'
const SETTINGS = join(repo, 'controlplane', 'settings', '.hypothesisloop.dev.yaml')

const flag = (name, fallback) => process.argv.includes(name) ? process.argv[process.argv.indexOf(name) + 1] : fallback
const until = flag('--until', 'done')
const off = flag('--off', '').split(',').filter(Boolean)
const RESEARCHERS = 2
const LONGEST = 60 * 60 * 1000
const REPOSITORY = process.env.HL_E2E_GITHUB_REPO
const INSTALLATION = process.env.HL_E2E_GITHUB_INSTALLATION
assert.ok(REPOSITORY && INSTALLATION, 'usage: HL_E2E_GITHUB_REPO=.. HL_E2E_GITHUB_INSTALLATION=.. node e2e/walk.mjs')

const run = randomUUID().slice(0, 6)
const person = { id: randomUUID(), name: `Walk ${run}` }
const notes = []
const note = text => { notes.push(text); console.log(`NOTE ${text}`) }
const passed = name => console.log(`STEP ${name}`)
const hlAdmin = (...args) => execFileSync('bash', ['-c', '. localdev/lib/auth.sh && lib_hl_admin "$@"', 'hl-admin', ...args], {
  cwd: repo, env: { ...process.env, HYPOTHESISLOOP_CONFIG: SETTINGS }, encoding: 'utf8',
})
const sleep = ms => new Promise(resolve => setTimeout(resolve, ms))

async function main() {
  const closers = []
  const idp = await startIdentityProvider({ port: 54399, keyPath: join(repo, 'localdev', '.dev-jwt-key.pem'), ui: UI })
  closers.push(() => idp.close())
  const proxy = await startProxy({ port: PROXY_PORT, from: { host: 'localhost', port: 3000 }, to: { host: 'localhost', port: UI_PORT } })
  closers.push(() => proxy.close())
  const ui = spawn(process.execPath, [join(here, '..', 'node_modules', 'next', 'dist', 'bin', 'next'), 'dev', '-p', String(UI_PORT)], {
    cwd: join(here, '..'),
    env: { ...process.env, NEXT_DIST_DIR: UI_BUILD, NEXT_PUBLIC_API_URL: API, NEXT_PUBLIC_IDENTITY_PROVIDER: 'supabase', NEXT_PUBLIC_IDENTITY_SUPABASE_URL: IDP,
      NEXT_PUBLIC_IDENTITY_SUPABASE_PUBLISHABLE_KEY: 'sb_publishable_browser-run', NEXT_PUBLIC_SIGN_IN_PROVIDERS: 'github' },
    stdio: ['ignore', 'ignore', 'inherit'],
  })
  closers.push(async () => {
    if (ui.exitCode === null) { ui.kill(); await once(ui, 'exit') }
    rmSync(join(here, '..', UI_BUILD), { recursive: true, force: true, maxRetries: 5, retryDelay: 200 })
  })
  let experimentID
  let token
  try {
    for (let i = 0; i < 120 && !(await fetch(`http://localhost:${UI_PORT}/login`).catch(() => null))?.ok; i++) await sleep(1000)
    const browser = await chromium.launch({ proxy: { server: `http://127.0.0.1:${PROXY_PORT}`, bypass: '<-loopback>' } })
    closers.push(() => browser.close())
    const context = await browser.newContext({ viewport: { width: 1440, height: 900 }, reducedMotion: 'reduce' })
    const page = await context.newPage()
    page.setDefaultTimeout(60000)
    const problems = []
    page.on('console', message => { if (message.type() === 'error') problems.push(`console: ${message.text().slice(0, 300)}`) })
    page.on('response', response => { if (response.status() >= 400 && response.url().startsWith(API)) problems.push(`${response.status()} ${response.request().method()} ${response.url()}`) })
    context.on('weberror', e => problems.push(`crash: ${e.error().message}`))

    // Sign in.
    await fetch(`${IDP}/test/configure`, { method: 'POST', body: JSON.stringify({ nextUser: person }) })
    token = (await (await fetch(`${IDP}/test/token`, { method: 'POST', body: JSON.stringify({ user: person }) })).json()).access_token
    await page.goto(UI)
    await page.waitForLoadState('networkidle')
    passed('first-page')
    await page.getByRole('link', { name: 'Sign in' }).first().click()
    await page.waitForLoadState('networkidle')
    passed('login')
    await page.getByRole('button', { name: /github/i }).click()
    await page.waitForURL(url => !url.pathname.startsWith('/login') && !url.pathname.startsWith('/auth'), { timeout: 60000 })
    await page.waitForLoadState('networkidle')
    console.log(`landed on ${page.url()}`)
    passed('after-sign-in')

    // The org's GitHub account, before and after it is connected.
    const me = await (await fetch(`${API}/me`, { headers: { Authorization: `Bearer ${token}` } })).json()
    const org = me.orgs[0]
    console.log(`org ${org.id} "${org.name}", project "${me.projects[0].name}"`)
    const project = me.projects[0]
    await page.goto(`${UI}/orgs/${org.id}/settings`)
    await page.waitForLoadState('networkidle')
    passed('settings-before-github')
    hlAdmin('github', 'connect', org.id, INSTALLATION)
    note('GitHub was connected with hl-admin: the Connect GitHub button needs a person at GitHub')
    await page.reload()
    await page.waitForLoadState('networkidle')
    passed('settings-github-and-clusters')
    await page.goto(`${UI}/orgs/${org.id}/clusters`)
    await page.waitForLoadState('networkidle')
    for (const cluster of off) {
      const use = page.getByRole('switch', { name: `Use ${cluster} for the jobs of ${org.name}` })
      if (await use.count() === 0) { note(`Compute Resources lists no switch for ${cluster}`); continue }
      if (await use.isChecked()) await use.click()
      await page.waitForFunction(label => document.querySelector(`[aria-label="${label}"]`)?.getAttribute('aria-checked') === 'false', `Use ${cluster} for the jobs of ${org.name}`)
    }
    passed('compute-resources')
    await page.goto(`${UI}/projects/${project.id}/experiments`)
    await page.waitForLoadState('networkidle')
    passed('console')
    if (until === 'form') return

    // The home page starts from a sentence; the form this walk fills is behind Advanced.
    await page.getByRole('button', { name: 'Advanced', exact: true }).click()
    const form = page.getByRole('dialog')
    await form.getByLabel('Repository').click()
    await page.getByRole('option', { name: REPOSITORY }).click()
    await form.getByText(/Filled in from|has no \.hypothesisloop|Could not read/).waitFor()
    passed('form-repository-chosen')
    await form.getByRole('button', { name: 'Next: budget and rules' }).click()
    await form.getByRole('button', { name: 'Hosted agents' }).click()
    await form.getByLabel('Researchers').fill(String(RESEARCHERS))
    passed('form-competition-settings')
    if (until === 'filled') return
    await form.getByRole('button', { name: 'Create and run' }).click()
    await page.waitForTimeout(700)
    passed('pressed-create-and-run')
    await page.waitForURL(/\/experiments\/pe-/, { timeout: 120000 })
    experimentID = new URL(page.url()).pathname.split('/').pop()
    console.log(`experiment ${experimentID}`)
    const started = Date.now()
    const read = async path => (await fetch(`${API}${path}`, { headers: { Authorization: `Bearer ${token}` } })).json()
    const marks = {}
    const mark = async name => {
      if (marks[name]) return
      marks[name] = Math.round((Date.now() - started) / 1000)
      console.log(`MARK ${name} at ${marks[name]}s`)
      await page.waitForTimeout(1500)
      passed(name)
    }
    await mark('just-created')
    if (until === 'created') return
    for (const at of [10, 25]) { await sleep((at * 1000) - (Date.now() - started)); passed(`after-${at}s`) }
    let lastPrinted = Date.now()
    while (Date.now() - started < LONGEST) {
      const pe = await read(`/platform-experiments/${experimentID}`)
      const agents = (await read(`/platform-experiments/${experimentID}/agents`)).agents ?? []
      const jobs = await read(`/experiments?platform_experiment_id=${experimentID}&limit=50`)
      const instances = [pe.coordinator?.instance, ...agents.map(a => a.instance)].filter(Boolean)
      if (instances.some(i => i.state === 'starting')) await mark('a-machine-starting')
      if (instances.length > RESEARCHERS && instances.every(i => i.state === 'running')) await mark('all-machines-running')
      if (instances.some(i => i.progress_at)) await mark('first-progress')
      if (jobs.length > 0) await mark('first-job')
      if (jobs.some(j => j.status === 'RUNNING')) await mark('job-running')
      if (jobs.some(j => j.status === 'COMPLETED')) await mark('job-completed')
      if (pe.summary) await mark('summary')
      if (instances.some(i => i.stopped)) await mark('an-agent-stopped')
      if (pe.status === 'closed') { await mark('closed'); break }
      if (Date.now() - lastPrinted > 5 * 60 * 1000) { lastPrinted = Date.now(); passed(`at-${Math.round((Date.now() - started) / 60000)}min`) }
      await sleep(10000)
    }
    const ends = { jobs: `/projects/${project.id}/jobs`, hypotheses: `/projects/${project.id}/hypotheses`, cluster: `/orgs/${org.id}/clusters`, 'platform-experiments': `/projects/${project.id}/experiments` }
    for (const [name, path] of Object.entries(ends)) {
      await page.goto(`${UI}${path}`)
      await page.waitForLoadState('networkidle')
      passed(`end-${name}`)
    }
    console.log(`MARKS ${JSON.stringify(marks)}`)
    if (problems.length) console.log(`PROBLEMS\n${[...new Set(problems)].join('\n')}`)
  } finally {
    if (experimentID && token) {
      await fetch(`${API}/platform-experiments/${experimentID}/close`, { method: 'POST', headers: { Authorization: `Bearer ${token}` } })
    }
    console.log(`NOTES\n${notes.join('\n')}`)
    for (const close of closers.reverse()) await Promise.resolve().then(close).catch(error => console.error(error))
  }
}

// An open keep-alive connection of the proxy would otherwise hold the process.
main().then(() => process.exit(0), error => { console.error(error); process.exit(1) })

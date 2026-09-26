// Walks the path of a new person on the hosted stack: sign in, open the full form behind
// Advanced on the home page, pick a repository, Create and run,
// and watch the experiment. It rents real machines, so it is run by hand:
//   node e2e/walk.mjs [--until form|filled|created|done] [--off cluster,cluster] [--no-repository]
// --no-repository connects no GitHub account and types the task in: agents start in an empty
// workspace and the platform keeps their code.
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
const noRepository = process.argv.includes('--no-repository')
const RESEARCHERS = 2
// A task small enough to type: it needs no accelerator and nothing but Python.
const NO_REPOSITORY_BRIEF = `There is no repository: start from an empty directory.
Write sort.py, a function that sorts one million random 32-bit integers (seed 0) in plain Python
without calling sorted or list.sort, and bench.py, which checks the result against sorted and
reports items_per_second (higher is better) to the platform. A wrong order fails the job.
Each agent tries a different algorithm, one job at a time, at most three jobs, each asking for
2 CPU cores (cpu: 2 and no accelerator) and 4Gi. The coordinator closes the experiment once both have stopped.`
const LONGEST = 60 * 60 * 1000
const REPOSITORY = process.env.HL_E2E_GITHUB_REPO
const INSTALLATION = process.env.HL_E2E_GITHUB_INSTALLATION
assert.ok(noRepository || (REPOSITORY && INSTALLATION), 'usage: HL_E2E_GITHUB_REPO=.. HL_E2E_GITHUB_INSTALLATION=.. node e2e/walk.mjs, or --no-repository')

const run = randomUUID().slice(0, 6)
const person = { id: randomUUID(), name: `Walk ${run}` }
const notes = []
const note = text => { notes.push(text); console.log(`NOTE ${text}`) }
const shot = async (page, name) => console.log(`STEP ${name}`)
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
    env: { ...process.env, NEXT_DIST_DIR: UI_BUILD, NEXT_PUBLIC_API_URL: API, NEXT_PUBLIC_SUPABASE_URL: IDP,
      NEXT_PUBLIC_SUPABASE_PUBLISHABLE_KEY: 'sb_publishable_browser-run', NEXT_PUBLIC_SIGN_IN_PROVIDERS: 'github' },
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
    await shot(page, 'first-page')
    await page.getByRole('link', { name: 'Sign in' }).first().click()
    await page.waitForLoadState('networkidle')
    await shot(page, 'login')
    await page.getByRole('button', { name: /github/i }).click()
    await page.waitForURL(url => !url.pathname.startsWith('/login') && !url.pathname.startsWith('/auth'), { timeout: 60000 })
    await page.waitForLoadState('networkidle')
    console.log(`landed on ${page.url()}`)
    await shot(page, 'after-sign-in')

    // The org's GitHub account, before and after it is connected.
    const me = await (await fetch(`${API}/me`, { headers: { Authorization: `Bearer ${token}` } })).json()
    const org = me.orgs[0]
    console.log(`org ${org.id} "${org.name}", project "${me.projects[0].name}"`)
    await page.goto(`${UI}/settings`)
    await page.waitForLoadState('networkidle')
    await shot(page, 'settings-before-github')
    if (!noRepository) {
      hlAdmin('github', 'connect', org.id, INSTALLATION)
      note('GitHub was connected with hl-admin: the Connect GitHub button needs a person at GitHub')
    }
    await page.reload()
    await page.waitForLoadState('networkidle')
    for (const cluster of off) {
      const use = page.getByRole('switch', { name: `Use ${cluster} for the jobs of ${org.name}` })
      if (await use.count() === 0) { note(`Settings lists no switch for ${cluster}`); continue }
      if (await use.isChecked()) await use.click()
      await page.waitForFunction(label => document.querySelector(`[aria-label="${label}"]`)?.getAttribute('aria-checked') === 'false', `Use ${cluster} for the jobs of ${org.name}`)
    }
    await shot(page, 'settings-github-and-clusters')
    await page.goto(`${UI}/cluster`)
    await page.waitForLoadState('networkidle')
    await shot(page, 'compute-resources')
    await page.goto(`${UI}/platform-experiments`)
    await page.waitForLoadState('networkidle')
    await shot(page, 'console')
    if (until === 'form') return

    // The home page starts from a sentence; the form this walk fills is behind Advanced.
    await page.getByRole('button', { name: 'Advanced', exact: true }).click()
    const form = page.getByRole('dialog')
    if (noRepository) {
      await form.getByPlaceholder('e.g. Q3 Vision Benchmark').fill(`walk-${run}`)
      await form.getByLabel('Description').fill(NO_REPOSITORY_BRIEF)
      await form.getByPlaceholder('custom_metric_key').fill('items_per_second')
      await form.getByRole('button', { name: 'Add Custom' }).click()
    } else {
      await form.getByLabel('Repository').click()
      await page.getByRole('option', { name: REPOSITORY }).click()
      await form.getByText(/Filled in from|has no \.hypothesisloop|Could not read/).waitFor()
    }
    await shot(page, 'form-repository-chosen')
    await form.getByRole('button', { name: 'Next: budget and rules' }).click()
    await form.getByRole('button', { name: 'Hosted agents' }).click()
    await form.getByLabel('Researchers').fill(String(RESEARCHERS))
    await shot(page, 'form-competition-settings')
    if (until === 'filled') return
    await form.getByRole('button', { name: 'Create and run' }).click()
    await page.waitForTimeout(700)
    await shot(page, 'pressed-create-and-run')
    await page.waitForURL(/\/platform-experiments\/pe-/, { timeout: 120000 })
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
      await shot(page, name)
    }
    await mark('just-created')
    if (until === 'created') return
    for (const at of [10, 25]) { await sleep((at * 1000) - (Date.now() - started)); await shot(page, `after-${at}s`) }
    let lastShot = Date.now()
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
      if (Date.now() - lastShot > 5 * 60 * 1000) { lastShot = Date.now(); await shot(page, `at-${Math.round((Date.now() - started) / 60000)}min`) }
      await sleep(10000)
    }
    for (const path of ['/jobs', '/hypotheses', '/agents', '/cluster', '/statistics', '/platform-experiments']) {
      await page.goto(`${UI}${path}`)
      await page.waitForLoadState('networkidle')
      await shot(page, `end${path.replace('/', '-')}`)
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

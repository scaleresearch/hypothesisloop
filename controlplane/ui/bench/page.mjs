// Times the real hypotheses page of a seeded experiment in Chromium, signed in through the
// graph-1k stack's stand-in identity provider (localdev/graph-1k): opening it, filtering, selecting
// an idea, zooming, panning, and the polls while ideas arrive and verdicts change. Records a
// Chrome trace of the throttled run and screenshots of the graph. Run from controlplane/ui:
//   node bench/page.mjs EXPERIMENT_ID [OUT_DIR]
// THROTTLE=<n> slows the CPU n times (default 4). UI_URL, API_URL and IDP_URL default to the
// graph-1k stack's.
import { mkdirSync, writeFileSync } from 'node:fs'
import { join } from 'node:path'
import { chromium } from 'playwright'

const [experiment, out = '/tmp/graph-1k/run'] = process.argv.slice(2)
if (!experiment) throw new Error('usage: node bench/page.mjs EXPERIMENT_ID [OUT_DIR]')
const UI = process.env.UI_URL ?? 'http://localhost:3300'
const API = process.env.API_URL ?? 'http://localhost:8181'
const IDP = process.env.IDP_URL ?? 'http://127.0.0.1:54499'
const THROTTLE = Number(process.env.THROTTLE ?? 4)
const READER = { id: '8a1f3c2e-0000-4000-8000-000000000001', name: 'Graph Reader' }
mkdirSync(out, { recursive: true })

const post = (url, body) => fetch(url, { method: 'POST', body: JSON.stringify(body) })
const token = (await (await post(`${IDP}/test/token`, { user: READER })).json()).access_token
async function api(method, path, body) {
  const res = await fetch(`${API}${path}`, {
    method,
    headers: { Authorization: `Bearer ${token}`, ...(body ? { 'Content-Type': 'application/json' } : {}) },
    body: body ? JSON.stringify(body) : undefined,
  })
  if (!res.ok) throw new Error(`${method} ${path}: ${res.status} ${await res.text()}`)
  return res.json()
}

const browser = await chromium.launch({ headless: !process.env.HEADED })
const context = await browser.newContext({ viewport: { width: 1440, height: 900 } })
// Every long task and every frame the page draws, from its first script on.
await context.addInitScript(() => {
  window.__long = []
  new PerformanceObserver(list => {
    for (const e of list.getEntries()) window.__long.push({ start: e.startTime, duration: e.duration })
  }).observe({ type: 'longtask', buffered: true })
  window.__frames = []
  const frame = t => {
    window.__frames.push(t)
    requestAnimationFrame(frame)
  }
  requestAnimationFrame(frame)
})
const page = await context.newPage()
page.on('pageerror', e => console.error('page error:', e.message))
const cdp = await context.newCDPSession(page)
await cdp.send('Performance.enable')

await post(`${IDP}/test/configure`, { nextUser: READER })
await page.goto(`${UI}/login`)
await page.getByRole('button', { name: 'Continue with GitHub' }).click()
await page.waitForURL(u => !u.pathname.startsWith('/login') && !u.pathname.startsWith('/auth'))
// next dev compiles a page on its first visit: that is not the page's own cost.
await page.goto(`${UI}/experiments/${experiment}/hypotheses`)
await page.locator('.react-flow__node').first().waitFor({ timeout: 120_000 })
await page.evaluate(() => document.querySelector('.react-flow').scrollIntoView({ block: 'end' }))

const now = () => page.evaluate(() => performance.now())
const longSince = since => page.evaluate(s => window.__long.filter(l => l.start >= s), since)
const framesSince = since => page.evaluate(s => window.__frames.filter(t => t >= s), since)
const pct = (xs, p) => xs.length ? [...xs].sort((a, b) => a - b)[Math.min(xs.length - 1, Math.floor(xs.length * p))] : 0
function frameStats(frames) {
  const gaps = frames.slice(1).map((t, i) => t - frames[i])
  return { frames: gaps.length, p50: pct(gaps, 0.5), p95: pct(gaps, 0.95), max: Math.max(0, ...gaps) }
}
function longStats(long) {
  return { count: long.length, over100: long.filter(l => l.duration > 100).length, max: Math.max(0, ...long.map(l => l.duration)), total: long.reduce((a, l) => a + l.duration, 0) }
}
async function heap() {
  const { metrics } = await cdp.send('Performance.getMetrics')
  return Math.round(metrics.find(m => m.name === 'JSHeapUsedSize').value / 2 ** 20)
}
async function quiet(ms = 600) {
  // Until the main thread has gone ms without a long task.
  for (;;) {
    const t = await now()
    await page.waitForTimeout(ms)
    if ((await longSince(t)).length === 0) return
  }
}
const pane = () => page.locator('.react-flow__pane').boundingBox()

async function wheel(deltaY, steps) {
  const box = await pane()
  await page.mouse.move(box.x + box.width / 2, box.y + box.height / 2)
  for (let i = 0; i < steps; i++) {
    await page.mouse.wheel(0, deltaY)
    await page.waitForTimeout(16)
  }
}
async function drag(dx, dy, steps) {
  const box = await pane()
  const x = box.x + box.width / 2
  const y = box.y + box.height / 2
  await page.mouse.move(x, y)
  await page.mouse.down()
  for (let i = 1; i <= steps; i++) await page.mouse.move(x + (dx * i) / steps, y + (dy * i) / steps)
  await page.mouse.up()
}

async function gestures(label) {
  const result = {}
  let t = await now()
  await wheel(-60, 20)
  await wheel(60, 20)
  result.zoom = { ...frameStats(await framesSince(t)), long: longStats(await longSince(t)) }
  await quiet()
  t = await now()
  await drag(-500, -250, 40)
  await drag(500, 250, 40)
  result.pan = { ...frameStats(await framesSince(t)), long: longStats(await longSince(t)) }
  await quiet()
  console.log(label, JSON.stringify(result))
  return result
}

const report = { throttle: THROTTLE, experiment }
await cdp.send('Emulation.setCPUThrottlingRate', { rate: THROTTLE })
await cdp.send('Tracing.start', {
  categories: 'devtools.timeline,disabled-by-default-devtools.timeline,disabled-by-default-devtools.timeline.frame,v8.execute,blink.user_timing,latencyInfo',
  transferMode: 'ReturnAsStream',
})

// First open: navigation to the graph drawn and the main thread quiet.
const navStart = Date.now()
await page.goto(`${UI}/experiments/${experiment}/hypotheses`)
await page.locator('.react-flow__node').first().waitFor({ timeout: 120_000 })
await page.evaluate(() => document.querySelector('.react-flow').scrollIntoView({ block: 'end' }))
const drawn = Date.now() - navStart
await quiet()
const long0 = await page.evaluate(() => window.__long)
const lastLong = long0.length ? Math.max(...long0.map(l => l.start + l.duration)) : 0
const navigationStart = await page.evaluate(() => performance.timeOrigin)
report.open = { graphDrawnMs: drawn, interactiveMs: Math.round(lastLong), long: longStats(long0), heapMB: await heap(), nodesInDom: await page.locator('.react-flow__node').count() }
console.log('open', JSON.stringify(report.open), navigationStart ? '' : '')

// Filtering: narrow to refuted, then back to all, through the Status field.
async function timed(act, until) {
  const t = await now()
  const t0 = Date.now()
  await act()
  await until()
  const ms = Date.now() - t0
  await quiet()
  return { ms, long: longStats(await longSince(t)) }
}
const statusField = page.getByRole('combobox', { name: 'Status' })
report.filter = await timed(async () => {
  await statusField.click()
  await page.getByRole('option', { name: 'refuted', exact: true }).click()
}, () => page.waitForURL(/status=refuted/))
report.unfilter = await timed(async () => {
  await statusField.click()
  await page.getByRole('option', { name: 'All', exact: true }).click()
}, () => page.waitForURL(u => !u.search.includes('status=')))
console.log('filter', JSON.stringify(report.filter), 'unfilter', JSON.stringify(report.unfilter))

// Selecting an idea drawn near the middle opens its details beside the graph.
const box = await pane()
const target = await page.evaluate(({ cx, cy }) => {
  let best = null
  for (const n of document.querySelectorAll('.react-flow__node-hypothesis')) {
    const r = n.getBoundingClientRect()
    const x = r.x + r.width / 2
    const y = r.y + r.height / 2
    if (!n.contains(document.elementFromPoint(x, y))) continue
    const d = Math.hypot(x - cx, y - cy)
    if (!best || d < best.d) best = { x, y, d }
  }
  return best
}, { cx: box.x + box.width / 2, cy: box.y + box.height / 2 })
report.select = await timed(
  () => page.mouse.click(target.x, target.y),
  () => page.locator('aside').getByText('Refines', { exact: false }).first().waitFor(),
)
console.log('select', JSON.stringify(report.select))
await page.goto(`${UI}/experiments/${experiment}/hypotheses`)
await page.locator('.react-flow__node').first().waitFor()
await page.evaluate(() => document.querySelector('.react-flow').scrollIntoView({ block: 'end' }))
await quiet()

report.gestures = await gestures('throttled')

// Live: ideas arrive and verdicts change through the API while the page polls every 15 s.
const graph = await api('GET', `/hypotheses/graph?platform_experiment_id=${experiment}`)
const owned = graph.nodes.filter(n => n.agent_id)
const pollCosts = []
page.on('response', async res => {
  if (!res.url().includes('/hypotheses/graph')) return
  const t = await now().catch(() => null)
  if (t === null) return
  await page.waitForTimeout(1500).catch(() => {})
  const long = await longSince(t).catch(() => [])
  pollCosts.push(longStats(long))
})
const liveStart = await now()
for (let i = 0; i < 6; i++) {
  const parent = owned[(i * 97) % owned.length]
  await api('POST', '/hypotheses', {
    platform_experiment_id: experiment,
    agent_id: parent.agent_id,
    text: `Live idea ${Date.now()}: ${['narrowing', 'widening', 'shortening'][i % 3]} the ${['warmup', 'schedule', 'mixture'][i % 3]} under this branch keeps its gain at ${i + 2}x the steps`,
    links: [{ type: 'builds_on', to: parent.id }],
    distinct_from: [],
  }).catch(e => console.error('live idea:', e.message))
  await page.waitForTimeout(7000)
}
await page.waitForTimeout(16_000)
report.live = { polls: pollCosts, long: longStats(await longSince(liveStart)), heapMB: await heap() }
console.log('live', JSON.stringify(report.live))

const { stream } = await new Promise(resolve => {
  cdp.once('Tracing.tracingComplete', resolve)
  cdp.send('Tracing.end')
})
const chunks = []
for (;;) {
  const { data, eof, base64Encoded } = await cdp.send('IO.read', { handle: stream })
  chunks.push(base64Encoded ? Buffer.from(data, 'base64').toString() : data)
  if (eof) break
}
await cdp.send('IO.close', { handle: stream })
writeFileSync(join(out, 'trace.json'), chunks.join(''))

// Unthrottled, the frame rate a reader gets on this machine.
await cdp.send('Emulation.setCPUThrottlingRate', { rate: 1 })
report.unthrottled = await gestures('unthrottled')

// Screenshots: as it opens, the whole graph, and zoomed into its densest part.
await page.goto(`${UI}/experiments/${experiment}/hypotheses`)
await page.locator('.react-flow__node').first().waitFor()
await page.evaluate(() => document.querySelector('.react-flow').scrollIntoView({ block: 'end' }))
await quiet()
await page.screenshot({ path: join(out, 'default.png') })
await page.getByRole('button', { name: 'Fit View' }).click()
await quiet(1000)
await page.screenshot({ path: join(out, 'whole.png') })
const dense = await page.evaluate(() => {
  const pts = [...document.querySelectorAll('.react-flow__node')].map(n => n.getBoundingClientRect()).map(r => [r.x + r.width / 2, r.y + r.height / 2])
  let best = null
  for (const [x, y] of pts) {
    const n = pts.filter(([a, b]) => Math.hypot(a - x, b - y) < 60).length
    if (!best || n > best.n) best = { x, y, n }
  }
  return best
})
await page.mouse.move(dense.x, dense.y)
for (let i = 0; i < 12; i++) {
  await page.mouse.wheel(0, -100)
  await page.waitForTimeout(50)
}
await quiet(1000)
await page.screenshot({ path: join(out, 'dense.png') })

writeFileSync(join(out, 'report.json'), JSON.stringify(report, null, 2))
console.log(`report, trace and screenshots in ${out}`)
await browser.close()

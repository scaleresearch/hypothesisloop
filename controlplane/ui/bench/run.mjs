// Renders the hypothesis graph canvas alone at several sizes in headless Chromium and times
// opening it, selecting, filtering, a poll that changes one node, a poll that adds one, zooming
// and panning, near the default zoom and zoomed out. Run from controlplane/ui:
//   node bench/run.mjs [sizes...]
// THROTTLE=<n> slows the CPU n times (default 4), PROD=1 times a production build.
import { fileURLToPath } from 'node:url'
import react from '@vitejs/plugin-react'
import { chromium } from 'playwright'
import { build, createServer, preview } from 'vite'

const ui = fileURLToPath(new URL('..', import.meta.url))
const sizes = process.argv.slice(2).map(Number)
const THROTTLE = Number(process.env.THROTTLE ?? 4)

// PROD=1 times a production build; otherwise the development build next dev serves.
const config = {
  configFile: false,
  root: fileURLToPath(new URL('.', import.meta.url)),
  plugins: [react()],
  resolve: { alias: { '@': `${ui}src` } },
  css: { postcss: ui },
  define: { 'process.env.NEXT_PUBLIC_API_URL': '"http://api.test"' },
  build: { outDir: '/tmp/hypothesis-graph-bench', emptyOutDir: true },
  server: { port: 0 },
  preview: { port: 0 },
  logLevel: 'error',
}
let server
if (process.env.PROD) {
  await build(config)
  server = await preview(config)
} else {
  server = await createServer(config)
  await server.listen()
}
const url = server.resolvedUrls.local[0]

const browser = await chromium.launch()
const page = await browser.newPage({ viewport: { width: 1440, height: 900 } })
page.on('pageerror', e => console.error('page error:', e.message))
const cdp = await page.context().newCDPSession(page)

// The time from a state change to the frame after it has painted.
const timeUpdate = (change) => page.evaluate(async (change) => {
  const frame = () => new Promise(r => requestAnimationFrame(() => r()))
  await frame()
  const t = performance.now()
  window.bench.set(new Function(`return ${change}`)())
  await frame()
  await new Promise(r => setTimeout(r, 0))
  return performance.now() - t
}, change)

// Dispatches one gesture event a frame for `frames` frames and reports the frame intervals.
const timeGesture = (kind) => page.evaluate(async (kind) => {
  const pane = document.querySelector('.react-flow__pane')
  const box = pane.getBoundingClientRect()
  const cx = box.left + box.width / 2
  const cy = box.top + box.height / 2
  const frames = 60
  const times = []
  if (kind === 'pan') pane.dispatchEvent(new MouseEvent('mousedown', { bubbles: true, clientX: cx, clientY: cy, button: 0, view: window }))
  await new Promise(resolve => {
    let i = 0
    const step = (t) => {
      times.push(t)
      if (i === frames) return resolve()
      if (kind === 'pan') {
        const dx = 6 * (i < frames / 2 ? i : frames - i)
        window.dispatchEvent(new MouseEvent('mousemove', { bubbles: true, clientX: cx + dx, clientY: cy + dx / 2, button: 0, buttons: 1, view: window }))
      } else {
        pane.dispatchEvent(new WheelEvent('wheel', { bubbles: true, cancelable: true, clientX: cx, clientY: cy, deltaY: i < frames / 2 ? 40 : -40, deltaMode: 0 }))
      }
      i++
      requestAnimationFrame(step)
    }
    requestAnimationFrame(step)
  })
  if (kind === 'pan') window.dispatchEvent(new MouseEvent('mouseup', { bubbles: true, clientX: cx, clientY: cy, view: window }))
  const gaps = times.slice(1).map((t, i) => t - times[i]).sort((a, b) => a - b)
  return { mean: gaps.reduce((a, b) => a + b, 0) / gaps.length, p95: gaps[Math.floor(gaps.length * 0.95)], max: gaps[gaps.length - 1] }
}, kind)

const fmt = x => x.toFixed(0).padStart(6)
console.log(`${process.env.PROD ? 'Production' : 'Development'} build, CPU throttled ${THROTTLE}x. Times in ms.`)
console.log('     n   open select filter  poll1   add1 | zoom mean/p95/max | pan mean/p95/max | zoomed out: zoom | pan')
for (const n of sizes.length ? sizes : [200, 500, 1000]) {
  await cdp.send('Emulation.setCPUThrottlingRate', { rate: 1 })
  await page.goto(`${url}?n=0`)
  await page.waitForFunction(() => window.bench)
  await cdp.send('Emulation.setCPUThrottlingRate', { rate: THROTTLE })
  const open = await timeUpdate(`b => ({ ...b, graph: window.bench.fixture(${n}) })`)
  await page.waitForTimeout(500)
  const select = await timeUpdate(`b => ({ ...b, selected: 'h-3' })`)
  const filter = await timeUpdate(`b => ({ ...b, filters: { status: 'open', owner: '' } })`)
  await timeUpdate(`b => ({ ...b, filters: { status: '', owner: '' } })`)
  const poll1 = await timeUpdate(`b => ({ ...b, graph: { ...structuredClone(b.graph), nodes: structuredClone(b.graph.nodes).map((h, i) => i === 3 ? { ...h, comment_count: h.comment_count + 1 } : h) } })`)
  const add1 = await timeUpdate(`b => { const g = structuredClone(b.graph); const h = { ...g.nodes[1], id: 'new', created_at: '2027-01-01T00:00:00Z', builds_on: [g.nodes[1].id] }; return { ...b, graph: { ...g, nodes: [...g.nodes, h], edges: [...g.edges, { ...g.edges[0], from: 'new', to: g.nodes[1].id }] } } }`)
  await page.waitForTimeout(300)
  const zoom = await timeGesture('zoom')
  await page.waitForTimeout(300)
  const pan = await timeGesture('pan')
  const drawn = await page.evaluate(() => document.querySelectorAll('.react-flow__node').length)
  // Zoomed out to a quarter of the default scale, where a big graph fills the screen with nodes.
  await page.evaluate(() => {
    const pane = document.querySelector('.react-flow__pane')
    const box = pane.getBoundingClientRect()
    pane.dispatchEvent(new WheelEvent('wheel', { bubbles: true, cancelable: true, clientX: box.left + box.width / 2, clientY: box.top + 50, deltaY: 1000 }))
  })
  await page.waitForTimeout(1000)
  const farZoom = await timeGesture('zoom')
  await page.waitForTimeout(300)
  const farPan = await timeGesture('pan')
  const farDrawn = await page.evaluate(() => document.querySelectorAll('.react-flow__node').length)
  console.log(`${String(n).padStart(6)} ${fmt(open)} ${fmt(select)} ${fmt(filter)} ${fmt(poll1)} ${fmt(add1)} | ${fmt(zoom.mean)}${fmt(zoom.p95)}${fmt(zoom.max)} | ${fmt(pan.mean)}${fmt(pan.p95)}${fmt(pan.max)}  (${drawn} drawn) | far ${fmt(farZoom.mean)}${fmt(farZoom.p95)}${fmt(farZoom.max)} | ${fmt(farPan.mean)}${fmt(farPan.p95)}${fmt(farPan.max)} (${farDrawn} drawn)`)
}
await browser.close()
await (server.close?.() ?? server.httpServer.close())

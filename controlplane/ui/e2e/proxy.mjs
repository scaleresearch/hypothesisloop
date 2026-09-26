import { createServer, request } from 'node:http'
import { connect } from 'node:net'

// A forward proxy for the browser of the run. What the browser asks of `from` is answered by
// `to` (both { host, port }), with the Host the browser sent; every other request goes where
// it was going.
export function startProxy({ port, from, to }) {
  const upstreamOf = (hostname, upstreamPort) => {
    const asked = { host: hostname.replace(/^\[|\]$/g, ''), port: upstreamPort }
    return asked.host === from.host && asked.port === from.port ? to : asked
  }

  const server = createServer((req, res) => {
    const url = new URL(req.url)
    const { 'proxy-connection': _, ...headers } = req.headers
    const upstream = request({ ...upstreamOf(url.hostname, Number(url.port) || 80), method: req.method, path: url.pathname + url.search, headers }, answer => {
      res.writeHead(answer.statusCode, answer.headers)
      answer.pipe(res)
    })
    upstream.on('error', error => {
      if (res.headersSent) return res.destroy()
      res.writeHead(502, { 'Content-Type': 'text/plain' }).end(`the e2e proxy could not reach ${url.host}: ${error.code ?? error.message}`)
    })
    req.pipe(upstream)
  })

  // A websocket (Next's reload channel, the API's watch) and https arrive as a tunnel.
  server.on('connect', (req, socket, head) => {
    const colon = req.url.lastIndexOf(':')
    const upstream = connect(upstreamOf(req.url.slice(0, colon), Number(req.url.slice(colon + 1))), () => {
      socket.write('HTTP/1.1 200 Connection Established\r\n\r\n')
      upstream.write(head)
      upstream.pipe(socket)
      socket.pipe(upstream)
    })
    upstream.on('error', () => socket.destroy())
    socket.on('error', () => upstream.destroy())
  })

  return new Promise((resolve, reject) => {
    server.once('error', reject)
    server.listen(port, '127.0.0.1', () => resolve(server))
  })
}

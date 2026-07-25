// A tiny BFF (backend-for-frontend): serves the built React app and proxies
// /api/* to the MCP backend inside the cluster. The browser only ever talks
// to this app's origin — no CORS, no cross-host logins, no mixed content.
import { createServer } from 'node:http'
import { readFile } from 'node:fs/promises'
import { extname, join, normalize } from 'node:path'
import { fileURLToPath } from 'node:url'

const port = Number(process.env.PORT || 3000)
const backend = process.env.BACKEND_URL || 'http://localhost:8000'
const dist = join(fileURLToPath(new URL('.', import.meta.url)), 'dist')

const types = {
  '.html': 'text/html; charset=utf-8',
  '.js': 'text/javascript',
  '.css': 'text/css',
  '.svg': 'image/svg+xml',
  '.png': 'image/png',
  '.ico': 'image/x-icon',
}

createServer(async (req, res) => {
  const url = new URL(req.url, 'http://localhost')

  if (url.pathname === '/api/health') {
    try {
      const upstream = await fetch(`${backend}/healthz`, { signal: AbortSignal.timeout(5000) })
      res.writeHead(upstream.status, { 'content-type': 'application/json' })
      res.end(await upstream.text())
    } catch {
      res.writeHead(502, { 'content-type': 'application/json' })
      res.end(JSON.stringify({ ok: false, error: 'backend unreachable' }))
    }
    return
  }

  // Static files with SPA fallback.
  const safePath = normalize(url.pathname).replace(/^(\.\.[/\\])+/, '')
  let file = join(dist, safePath)
  try {
    let body = await readFile(file)
    res.writeHead(200, { 'content-type': types[extname(file)] ?? 'application/octet-stream' })
    res.end(body)
  } catch {
    const index = await readFile(join(dist, 'index.html'))
    res.writeHead(200, { 'content-type': types['.html'] })
    res.end(index)
  }
}).listen(port, () => console.log(`frontend BFF on :${port}, backend at ${backend}`))

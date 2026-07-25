// A tiny BFF (backend-for-frontend): serves the built dashboard and proxies
// /api/* to the backend inside the cluster. The browser only ever talks to
// this app's origin — no CORS, no cross-host login, no mixed content. (The
// backend does enable CORS, but for MCP clients, not for this page.)
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

  if (url.pathname.startsWith('/api/')) {
    try {
      const upstream = await fetch(`${backend}${url.pathname}${url.search}`, {
        method: req.method,
        signal: AbortSignal.timeout(10_000),
      })
      res.writeHead(upstream.status, {
        'content-type': upstream.headers.get('content-type') ?? 'application/json',
      })
      res.end(Buffer.from(await upstream.arrayBuffer()))
    } catch {
      res.writeHead(502, { 'content-type': 'application/json' })
      res.end(JSON.stringify({ error: 'the backend is not answering' }))
    }
    return
  }

  // Static files, with the SPA's index.html as the fallback.
  const safePath = normalize(url.pathname).replace(/^(\.\.[/\\])+/, '')
  try {
    const body = await readFile(join(dist, safePath))
    res.writeHead(200, { 'content-type': types[extname(safePath)] ?? 'application/octet-stream' })
    res.end(body)
  } catch {
    res.writeHead(200, { 'content-type': types['.html'] })
    res.end(await readFile(join(dist, 'index.html')))
  }
}).listen(port, () => console.log(`dashboard on :${port}, backend at ${backend}`))

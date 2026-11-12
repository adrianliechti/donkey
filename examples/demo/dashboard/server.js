// Serves the built dashboard. Nothing else: /api and /mcp reach the backend
// through the gateway, because the backend claims those two paths of this
// app's address in its own donkey.yaml. One hostname, no proxy, no extra hop
// — and /mcp stays open for machine clients while this page keeps its login,
// because protection attaches per route.
import { createServer } from 'node:http'
import { readFile } from 'node:fs/promises'
import { extname, join, normalize } from 'node:path'
import { fileURLToPath } from 'node:url'

const port = Number(process.env.PORT || 3000)
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
}).listen(port, () => console.log(`dashboard on :${port}`))

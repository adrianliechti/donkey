// A guestbook: the page (frontend) and its API (backend) both live here, and
// everything they know about their services comes from the environment.
// DATABASE_URL and POCKETBASE_* exist because donkey.yaml lists the addons —
// no hostname, port or password appears anywhere in this folder.
const express = require('express')
const { Pool } = require('pg')

const app = express()
app.use(express.urlencoded({ extended: false }))

const pool = new Pool({ connectionString: process.env.DATABASE_URL })
const ready = pool.query(
  'CREATE TABLE IF NOT EXISTS notes (id serial PRIMARY KEY, text text NOT NULL, at timestamptz DEFAULT now())',
)

// --- PocketBase: every signed note is also logged there as a record, so
// both bindings do real work. The admin credentials come from the binding.
const pb = process.env.POCKETBASE_URL

async function pbToken() {
  const res = await fetch(`${pb}/api/collections/_superusers/auth-with-password`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({
      identity: process.env.POCKETBASE_ADMIN_EMAIL,
      password: process.env.POCKETBASE_ADMIN_PASSWORD,
    }),
  })
  if (!res.ok) throw new Error(`pocketbase auth: HTTP ${res.status}`)
  return (await res.json()).token
}

async function pbLog(text) {
  const token = await pbToken()
  const auth = { Authorization: token, 'Content-Type': 'application/json' }
  const collection = await fetch(`${pb}/api/collections/events`, { headers: auth })
  if (collection.status === 404) {
    const created = await fetch(`${pb}/api/collections`, {
      method: 'POST',
      headers: auth,
      body: JSON.stringify({ name: 'events', type: 'base', fields: [{ name: 'note', type: 'text' }] }),
    })
    if (!created.ok) throw new Error(`pocketbase collection: HTTP ${created.status}`)
  }
  const record = await fetch(`${pb}/api/collections/events/records`, {
    method: 'POST',
    headers: auth,
    body: JSON.stringify({ note: text }),
  })
  if (!record.ok) throw new Error(`pocketbase record: HTTP ${record.status}`)
}

async function pbCount() {
  const token = await pbToken()
  const res = await fetch(`${pb}/api/collections/events/records?perPage=1`, {
    headers: { Authorization: token },
  })
  if (!res.ok) return null
  return (await res.json()).totalItems
}

const esc = (s) => s.replace(/[&<>"']/g, (c) => `&#${c.charCodeAt(0)};`)

// The health route answers without the services: a database hiccup should
// show on the page, not as donkey restarting the app.
app.get('/healthz', (_req, res) => res.send('ok'))

app.get('/', async (_req, res) => {
  try {
    await ready
    const { rows } = await pool.query('SELECT text, at FROM notes ORDER BY id DESC LIMIT 20')
    const logged = await pbCount().catch(() => null)
    res.send(`<!doctype html>
<meta charset="utf-8"><title>guestbook</title>
<body style="font-family: system-ui; max-width: 40rem; margin: 3rem auto">
<h1>guestbook</h1>
<form method="post" action="/notes">
  <input name="text" placeholder="leave a note" required maxlength="200" autofocus>
  <button>Sign</button>
</form>
<ul>${rows.map((r) => `<li>${esc(r.text)} <small>${r.at.toISOString()}</small></li>`).join('')}</ul>
<p><small>${rows.length} notes in Postgres · ${
      logged === null ? 'PocketBase unreachable' : `${logged} events logged in PocketBase`
    }</small></p>
</body>`)
  } catch (err) {
    res.status(500).send(`<pre>${esc(String(err))}</pre>`)
  }
})

app.post('/notes', async (req, res) => {
  const text = (req.body.text || '').trim().slice(0, 200)
  if (text) {
    await ready
    await pool.query('INSERT INTO notes (text) VALUES ($1)', [text])
    // Best effort: the note is saved either way; the count on / tells.
    await pbLog(text).catch(() => {})
  }
  res.redirect('/')
})

app.listen(process.env.PORT || 3000)

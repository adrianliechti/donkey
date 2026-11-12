// A guestbook: the page (frontend) and its API (backend) both live here, and
// everything they know about their services comes from the environment.
// DATABASE_URL exists because donkey.yaml lists Postgres — no hostname, port
// or password appears anywhere in this folder.
const express = require('express')
const { Pool } = require('pg')

const app = express()
app.use(express.urlencoded({ extended: false }))

const pool = new Pool({ connectionString: process.env.DATABASE_URL })
const ready = pool.query(
  'CREATE TABLE IF NOT EXISTS notes (id serial PRIMARY KEY, text text NOT NULL, at timestamptz DEFAULT now())',
)

const esc = (s) => s.replace(/[&<>"']/g, (c) => `&#${c.charCodeAt(0)};`)

// The health route answers without the services: a database hiccup should
// show on the page, not as donkey restarting the app.
app.get('/healthz', (_req, res) => res.send('ok'))

app.get('/', async (_req, res) => {
  try {
    await ready
    const { rows } = await pool.query('SELECT text, at FROM notes ORDER BY id DESC LIMIT 20')
    res.send(`<!doctype html>
<meta charset="utf-8"><title>guestbook</title>
<body style="font-family: system-ui; max-width: 40rem; margin: 3rem auto">
<h1>guestbook</h1>
<form method="post" action="/notes">
  <input name="text" placeholder="leave a note" required maxlength="200" autofocus>
  <button>Sign</button>
</form>
<ul>${rows.map((r) => `<li>${esc(r.text)} <small>${r.at.toISOString()}</small></li>`).join('')}</ul>
<p><small>${rows.length} notes in Postgres</small></p>
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
  }
  res.redirect('/')
})

app.listen(process.env.PORT || 3000)

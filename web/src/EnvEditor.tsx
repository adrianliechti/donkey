import { useEffect, useState } from 'react'
import { Check, ClipboardCopy, Eye, EyeOff, Pencil, Plus, Trash2 } from 'lucide-react'
import { api } from './api'
import { Button, Card, ErrorNote, errText, SectionTitle, Spinner } from './ui'

const KEY_RE = /^[A-Za-z_][A-Za-z0-9_]*$/

export function EnvEditor({ space, app, onSaved }: { space: string; app: string; onSaved: () => void }) {
  const [env, setEnv] = useState<Record<string, string> | null>(null)
  const [dirty, setDirty] = useState(false)
  const [reveal, setReveal] = useState<Record<string, boolean>>({})
  const [revealAll, setRevealAll] = useState(false)
  const [editing, setEditing] = useState<string | null>(null)
  const [editValue, setEditValue] = useState('')
  const [newKey, setNewKey] = useState('')
  const [newValue, setNewValue] = useState('')
  const [pasteMode, setPasteMode] = useState(false)
  const [pasteText, setPasteText] = useState('')
  const [copied, setCopied] = useState('')
  const [busy, setBusy] = useState(false)
  const [note, setNote] = useState('')
  const [error, setError] = useState('')

  useEffect(() => {
    api
      .env(space, app)
      .then((res) => setEnv(res.env ?? {}))
      .catch((err) => setError(errText(err, 'Loading failed.')))
  }, [space, app])

  function validKey(key: string): string {
    if (!KEY_RE.test(key)) return `“${key}” is not a valid variable name — letters, digits and _ only.`
    if (key === 'PORT') return 'PORT is set by donkey from the `port` in donkey.yaml — change it there and deploy again.'
    return ''
  }

  function addVar(e?: React.FormEvent) {
    e?.preventDefault()
    const key = newKey.trim()
    if (!key || env === null) return
    const problem = validKey(key)
    if (problem) {
      setError(problem)
      return
    }
    if (key in env && env[key] !== newValue) {
      setNote(`${key} already existed — its value was replaced (not saved yet).`)
    } else {
      setNote('')
    }
    setEnv({ ...env, [key]: newValue })
    setNewKey('')
    setNewValue('')
    setDirty(true)
    setError('')
  }

  function addPasted() {
    if (env === null) return
    const next = { ...env }
    let added = 0
    for (const line of pasteText.split('\n')) {
      const trimmed = line.trim()
      if (trimmed === '' || trimmed.startsWith('#')) continue
      const eq = trimmed.indexOf('=')
      if (eq <= 0) continue
      const key = trimmed.slice(0, eq).trim()
      if (validKey(key)) continue
      next[key] = trimmed.slice(eq + 1)
      added++
    }
    if (added === 0) {
      setError('Nothing usable found — expected KEY=VALUE lines, like a .env file.')
      return
    }
    setEnv(next)
    setDirty(true)
    setPasteMode(false)
    setPasteText('')
    setNote(`${added} variable${added === 1 ? '' : 's'} added from the pasted text (not saved yet).`)
    setError('')
  }

  function removeVar(key: string) {
    if (env === null) return
    const next = { ...env }
    delete next[key]
    setEnv(next)
    setDirty(true)
    setNote('')
  }

  function startEdit(key: string) {
    if (env === null) return
    setEditing(key)
    setEditValue(env[key])
  }

  function commitEdit() {
    if (env === null || editing === null) return
    setEnv({ ...env, [editing]: editValue })
    setEditing(null)
    setDirty(true)
    setNote('')
  }

  async function copyValue(key: string) {
    if (env === null) return
    try {
      await navigator.clipboard.writeText(env[key])
      setCopied(key)
      setTimeout(() => setCopied(''), 1500)
    } catch {
      setError('Copying did not work — your browser blocked clipboard access.')
    }
  }

  async function save(restart: boolean) {
    if (env === null) return
    setBusy(true)
    setError('')
    try {
      const res = await api.setEnv(space, app, env, restart)
      setDirty(false)
      setNote(
        res.restarted
          ? `Saved ${res.count} variable${res.count === 1 ? '' : 's'} — the app is restarting with the new values.`
          : `Saved ${res.count} variable${res.count === 1 ? '' : 's'}. The running app keeps its old values until the next deploy or restart.`,
      )
      if (res.restarted) onSaved()
    } catch (err) {
      setError(errText(err, 'Saving did not work.'))
    } finally {
      setBusy(false)
    }
  }

  const keys = env ? Object.keys(env).sort() : []
  const shown = (key: string) => revealAll || reveal[key]

  return (
    <Card className="p-4">
      <div className="flex items-center justify-between">
        <SectionTitle>Environment variables</SectionTitle>
        <div className="flex items-center gap-3">
          <button
            onClick={() => setPasteMode(!pasteMode)}
            className="text-xs text-stone hover:text-ink focus-visible:outline-2 focus-visible:outline-hay-deep"
          >
            {pasteMode ? 'Single values' : 'Paste a .env'}
          </button>
          <button
            onClick={() => setRevealAll(!revealAll)}
            aria-pressed={revealAll}
            className="flex items-center gap-1 text-xs text-stone hover:text-ink focus-visible:outline-2 focus-visible:outline-hay-deep"
          >
            {revealAll ? (
              <>
                <EyeOff className="size-3.5" aria-hidden /> Hide all values
              </>
            ) : (
              <>
                <Eye className="size-3.5" aria-hidden /> Show all values
              </>
            )}
          </button>
        </div>
      </div>

      {env === null && !error && <Spinner label="Loading…" />}
      {env !== null && (
        <div className="mt-3 space-y-2">
          {keys.length === 0 && (
            <p className="text-sm text-stone">No variables yet — add the first one below.</p>
          )}
          {keys.map((key) => (
            <div key={key} className="flex flex-col gap-1 sm:flex-row sm:items-center sm:gap-2">
              <span className="truncate font-mono text-sm font-medium sm:w-1/3">{key}</span>
              {editing === key ? (
                <form
                  className="flex flex-1 items-center gap-2"
                  onSubmit={(e) => {
                    e.preventDefault()
                    commitEdit()
                  }}
                >
                  <input
                    value={editValue}
                    onChange={(e) => setEditValue(e.target.value)}
                    autoFocus
                    aria-label={`New value for ${key}`}
                    className="flex-1 rounded-md border border-line bg-surface px-2 py-1 font-mono text-sm focus:outline-2 focus:outline-hay-deep"
                  />
                  <Button kind="quiet" type="submit">
                    Apply
                  </Button>
                </form>
              ) : (
                <span className="flex-1 truncate font-mono text-sm text-ink-soft">
                  {shown(key) ? env[key] : '••••••••'}
                </span>
              )}
              <div className="flex items-center gap-0.5">
                <button
                  onClick={() => setReveal({ ...reveal, [key]: !shown(key) })}
                  aria-label={shown(key) ? `Hide ${key}` : `Show ${key}`}
                  aria-pressed={shown(key)}
                  className="rounded p-1 text-stone hover:bg-line/50 hover:text-ink focus-visible:outline-2 focus-visible:outline-hay-deep"
                >
                  {shown(key) ? <EyeOff className="size-4" aria-hidden /> : <Eye className="size-4" aria-hidden />}
                </button>
                <button
                  onClick={() => copyValue(key)}
                  aria-label={`Copy the value of ${key}`}
                  className="rounded p-1 text-stone hover:bg-line/50 hover:text-ink focus-visible:outline-2 focus-visible:outline-hay-deep"
                >
                  {copied === key ? (
                    <Check className="size-4 text-moss" aria-hidden />
                  ) : (
                    <ClipboardCopy className="size-4" aria-hidden />
                  )}
                </button>
                <button
                  onClick={() => startEdit(key)}
                  aria-label={`Edit ${key}`}
                  className="rounded p-1 text-stone hover:bg-line/50 hover:text-ink focus-visible:outline-2 focus-visible:outline-hay-deep"
                >
                  <Pencil className="size-4" aria-hidden />
                </button>
                <button
                  onClick={() => removeVar(key)}
                  aria-label={`Remove ${key}`}
                  className="rounded p-1 text-stone hover:bg-brick/10 hover:text-brick focus-visible:outline-2 focus-visible:outline-hay-deep"
                >
                  <Trash2 className="size-4" aria-hidden />
                </button>
              </div>
            </div>
          ))}

          {pasteMode ? (
            <div className="space-y-2 border-t border-line pt-3">
              <textarea
                value={pasteText}
                onChange={(e) => setPasteText(e.target.value)}
                placeholder={'DATABASE_URL=postgres://…\nSMTP_HOST=mail.example.com'}
                rows={4}
                aria-label="Paste KEY=VALUE lines"
                className="w-full rounded-md border border-line bg-surface px-2 py-1.5 font-mono text-sm focus:outline-2 focus:outline-hay-deep"
              />
              <div className="flex items-center justify-between">
                <p className="text-xs text-stone">One KEY=VALUE per line; # comments are skipped.</p>
                <Button kind="quiet" onClick={addPasted} disabled={pasteText.trim() === ''}>
                  Add these
                </Button>
              </div>
            </div>
          ) : (
            <form onSubmit={addVar} className="flex flex-col gap-2 border-t border-line pt-3 sm:flex-row sm:items-center">
              <input
                value={newKey}
                onChange={(e) => setNewKey(e.target.value)}
                placeholder="KEY"
                aria-label="Variable name"
                className="rounded-md border border-line bg-surface px-2 py-1.5 font-mono text-sm focus:outline-2 focus:outline-hay-deep sm:w-1/3"
              />
              <input
                value={newValue}
                onChange={(e) => setNewValue(e.target.value)}
                placeholder="value"
                aria-label="Variable value"
                className="flex-1 rounded-md border border-line bg-surface px-2 py-1.5 font-mono text-sm focus:outline-2 focus:outline-hay-deep"
              />
              <Button kind="quiet" type="submit" disabled={newKey.trim() === ''}>
                <span className="flex items-center gap-1">
                  <Plus className="size-3.5" aria-hidden /> Add
                </span>
              </Button>
            </form>
          )}

          <div className="flex flex-wrap items-center justify-between gap-2 pt-2">
            <p className="text-xs text-stone">Changes apply when you save.</p>
            <div className="flex gap-2">
              <Button kind="quiet" onClick={() => save(false)} disabled={!dirty || busy}>
                Save only
              </Button>
              <Button onClick={() => save(true)} disabled={!dirty || busy}>
                {busy ? 'Saving…' : 'Save & restart app'}
              </Button>
            </div>
          </div>
          {note && (
            <p role="status" className="text-sm text-moss">
              {note}
            </p>
          )}
          <ErrorNote message={error} />
        </div>
      )}
    </Card>
  )
}

import { useEffect, useState } from 'react'
import { Check, ClipboardCopy, Eye, EyeOff, Pencil, Plus, Trash2, X } from 'lucide-react'
import { api } from './api'
import { Button, ErrorNote, errText, Spinner } from './ui'

const KEY_RE = /^[A-Za-z_][A-Za-z0-9_]*$/
const ROW_ACTION =
  'rounded-md p-1.5 text-stone transition-colors hover:bg-line/50 hover:text-ink focus-visible:outline-2 focus-visible:outline-hay-deep'
const ROW_DANGER_ACTION =
  'rounded-md p-1.5 text-stone transition-colors hover:bg-brick/10 hover:text-brick focus-visible:outline-2 focus-visible:outline-hay-deep'
const COLUMN_INPUT =
  'min-w-0 bg-transparent px-3 py-2.5 font-mono text-sm placeholder:text-stone/60 focus:bg-raised/30 focus:outline-none focus:ring-1 focus:ring-inset focus:ring-hay-deep'

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
    <div className="space-y-3">
      <div className="flex flex-wrap items-center justify-end gap-3">
        <button
          type="button"
          onClick={() => setPasteMode(!pasteMode)}
          className="text-xs text-stone hover:text-ink focus-visible:outline-2 focus-visible:outline-hay-deep"
        >
          {pasteMode ? 'Add one' : 'Paste .env'}
        </button>
        <button
          type="button"
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

      {env === null && !error && <Spinner label="Loading…" />}
      {env !== null && (
        <>
          <div className="overflow-hidden border-y border-line/70">
            {keys.length === 0 ? (
              <p className="px-3 py-5 text-center text-sm text-stone">
                No variables yet — add the first one below.
              </p>
            ) : (
              <div className="divide-y divide-line/60">
                {keys.map((key) => (
                  <div
                    key={key}
                    className="grid min-h-11 grid-cols-[minmax(0,1fr)_minmax(0,1.6fr)_8rem] items-stretch transition-colors hover:bg-raised/20"
                  >
                    <div className="flex min-w-0 items-center px-3 py-2">
                      <span className="truncate font-mono text-sm font-medium" title={key}>
                        {key}
                      </span>
                    </div>
                    <div className="flex min-w-0 items-center border-l border-line/60 px-3 py-2">
                      {editing === key ? (
                        <form
                          className="w-full"
                          onSubmit={(e) => {
                            e.preventDefault()
                            commitEdit()
                          }}
                        >
                          <input
                            value={editValue}
                            onChange={(e) => setEditValue(e.target.value)}
                            onKeyDown={(e) => {
                              if (e.key === 'Escape') setEditing(null)
                            }}
                            autoFocus
                            aria-label={`New value for ${key}`}
                            className="w-full min-w-0 border-b border-hay/60 bg-transparent py-0.5 font-mono text-sm focus:border-hay focus:outline-none"
                          />
                        </form>
                      ) : (
                        <span className="min-w-0 truncate font-mono text-sm text-ink-soft">
                          {shown(key) ? (
                            env[key] || <span className="font-sans text-xs italic text-stone">empty</span>
                          ) : (
                            '••••••••'
                          )}
                        </span>
                      )}
                    </div>
                    <div
                      className="flex items-center gap-0.5 border-l border-line/60 px-2"
                      role="group"
                      aria-label={`Actions for ${key}`}
                    >
                      {editing === key ? (
                        <>
                          <button
                            type="button"
                            onClick={commitEdit}
                            title="Apply change"
                            aria-label={`Apply change to ${key}`}
                            className={ROW_ACTION}
                          >
                            <Check className="size-3.5 text-moss" aria-hidden />
                          </button>
                          <button
                            type="button"
                            onClick={() => setEditing(null)}
                            title="Cancel editing"
                            aria-label={`Cancel editing ${key}`}
                            className={ROW_ACTION}
                          >
                            <X className="size-3.5" aria-hidden />
                          </button>
                        </>
                      ) : (
                        <>
                          <button
                            type="button"
                            onClick={() => setReveal({ ...reveal, [key]: !shown(key) })}
                            title={shown(key) ? 'Hide value' : 'Show value'}
                            aria-label={shown(key) ? `Hide ${key}` : `Show ${key}`}
                            aria-pressed={shown(key)}
                            className={ROW_ACTION}
                          >
                            {shown(key) ? (
                              <EyeOff className="size-3.5" aria-hidden />
                            ) : (
                              <Eye className="size-3.5" aria-hidden />
                            )}
                          </button>
                          <button
                            type="button"
                            onClick={() => copyValue(key)}
                            title="Copy value"
                            aria-label={`Copy the value of ${key}`}
                            className={ROW_ACTION}
                          >
                            {copied === key ? (
                              <Check className="size-3.5 text-moss" aria-hidden />
                            ) : (
                              <ClipboardCopy className="size-3.5" aria-hidden />
                            )}
                          </button>
                          <button
                            type="button"
                            onClick={() => startEdit(key)}
                            title="Edit value"
                            aria-label={`Edit ${key}`}
                            className={ROW_ACTION}
                          >
                            <Pencil className="size-3.5" aria-hidden />
                          </button>
                          <button
                            type="button"
                            onClick={() => removeVar(key)}
                            title="Remove variable"
                            aria-label={`Remove ${key}`}
                            className={ROW_DANGER_ACTION}
                          >
                            <Trash2 className="size-3.5" aria-hidden />
                          </button>
                        </>
                      )}
                    </div>
                  </div>
                ))}
              </div>
            )}

            {pasteMode ? (
              <div className="border-t border-line/70">
                <textarea
                  value={pasteText}
                  onChange={(e) => setPasteText(e.target.value)}
                  placeholder={'DATABASE_URL=postgres://…\nSMTP_HOST=mail.example.com'}
                  rows={4}
                  aria-label="Paste KEY=VALUE lines"
                  className="block w-full resize-y bg-transparent px-3 py-2.5 font-mono text-sm placeholder:text-stone/60 focus:bg-raised/30 focus:outline-none focus:ring-1 focus:ring-inset focus:ring-hay-deep"
                />
                <div className="flex flex-wrap items-center justify-between gap-2 border-t border-line/60 px-3 py-2">
                  <p className="text-xs text-stone">One KEY=VALUE per line; # comments are skipped.</p>
                  <Button kind="quiet" onClick={addPasted} disabled={pasteText.trim() === ''}>
                    Add these
                  </Button>
                </div>
              </div>
            ) : (
              <form
                onSubmit={addVar}
                className="grid grid-cols-[minmax(0,1fr)_minmax(0,1.6fr)_8rem] items-stretch border-t border-line/70"
              >
                <input
                  value={newKey}
                  onChange={(e) => setNewKey(e.target.value)}
                  placeholder="KEY"
                  aria-label="Variable name"
                  className={COLUMN_INPUT}
                />
                <input
                  value={newValue}
                  onChange={(e) => setNewValue(e.target.value)}
                  placeholder="value"
                  aria-label="Variable value"
                  className={`${COLUMN_INPUT} border-l border-line/60`}
                />
                <div className="flex items-center border-l border-line/60 px-2">
                  <button
                    type="submit"
                    disabled={newKey.trim() === ''}
                    className="inline-flex items-center gap-1 rounded-md px-2 py-1.5 text-xs font-medium text-ink-soft transition-colors hover:bg-line/50 hover:text-ink disabled:pointer-events-none disabled:opacity-35 focus-visible:outline-2 focus-visible:outline-hay-deep"
                  >
                    <Plus className="size-3.5" aria-hidden /> Add
                  </button>
                </div>
              </form>
            )}
          </div>

          <div className="flex flex-wrap items-center justify-between gap-2 pt-1">
            <p className={`text-xs ${dirty ? 'text-ink-soft' : 'text-stone'}`}>
              {dirty ? 'Unsaved changes' : 'Changes apply when you save.'}
            </p>
            <div className="flex gap-2">
              <Button kind="quiet" onClick={() => save(false)} disabled={!dirty || busy}>
                Save only
              </Button>
              <Button onClick={() => save(true)} disabled={!dirty || busy}>
                {busy ? 'Saving…' : 'Save & restart'}
              </Button>
            </div>
          </div>
          {note && (
            <p role="status" className="text-sm text-moss">
              {note}
            </p>
          )}
        </>
      )}
      <ErrorNote message={error} />
    </div>
  )
}

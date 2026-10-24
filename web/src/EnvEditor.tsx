import { useEffect, useState } from 'react'
import { Check, ClipboardCopy, Eye, EyeOff, Plus, Trash2 } from 'lucide-react'
import { api } from './api'
import { Button, ErrorNote, IconButton, SectionHeader, Spinner, SuccessNote } from './ui'
import { errText } from './format'

const KEY_RE = /^[A-Za-z_][A-Za-z0-9_]*$/
// Row actions stay out of sight until the row is hovered or focused: with
// many variables, a button per cell is noise.
const ROW_ACTION =
  'flex size-7 items-center justify-center rounded-md text-muted transition-colors hover:bg-line/50 hover:text-ink focus-visible:outline-2 focus-visible:outline-accent-strong'
const ROW_DANGER_ACTION =
  'flex size-7 items-center justify-center rounded-md text-muted transition-colors hover:bg-danger/10 hover:text-danger focus-visible:outline-2 focus-visible:outline-accent-strong'
const COLUMN_INPUT =
  'min-w-0 bg-transparent px-3 py-2.5 font-mono text-sm placeholder:text-muted/60 focus:bg-raised/30 focus:outline-none focus:ring-1 focus:ring-inset focus:ring-accent-strong'

export function EnvEditor({ space, app, onSaved }: { space: string; app: string; onSaved: () => void }) {
  const [env, setEnv] = useState<Record<string, string> | null>(null)
  const [dirty, setDirty] = useState(false)
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
    if (env[editing] !== editValue) {
      setEnv({ ...env, [editing]: editValue })
      setDirty(true)
      setNote('')
    }
    setEditing(null)
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

  return (
    <section className="space-y-3">
      <SectionHeader
        title="Variables"
        actions={
          <>
            <button
              type="button"
              onClick={() => setPasteMode(!pasteMode)}
              aria-pressed={pasteMode}
              className={`rounded px-1 text-xs text-muted underline-offset-2 transition-colors hover:text-ink hover:underline focus-visible:outline-2 focus-visible:outline-accent-strong ${pasteMode ? 'text-ink underline' : ''}`}
            >
              {pasteMode ? 'Add one at a time' : 'Paste .env'}
            </button>
            <IconButton
              icon={revealAll ? EyeOff : Eye}
              label={revealAll ? 'Hide values' : 'Show values'}
              active={revealAll}
              onClick={() => setRevealAll(!revealAll)}
            />
          </>
        }
      />

      {env === null && !error && <Spinner label="Loading…" />}
      {env !== null && (
        <>
          <div className="overflow-hidden rounded-lg border border-line">
            {keys.length > 0 && (
              <div className="divide-y divide-line/60">
                {keys.map((key) => (
                  <div
                    key={key}
                    className="group grid min-h-11 grid-cols-[minmax(0,1fr)_minmax(0,1.6fr)_4.5rem] items-stretch transition-colors hover:bg-raised/20"
                  >
                    <div className="flex min-w-0 items-center px-3 py-2">
                      <span className="truncate font-mono text-sm font-medium" title={key}>
                        {key}
                      </span>
                    </div>
                    <div className="flex min-w-0 items-center border-l border-line/60">
                      {editing === key ? (
                        // The cell is the editor: Enter applies, Escape
                        // cancels, leaving the field applies too.
                        <input
                          value={editValue}
                          onChange={(e) => setEditValue(e.target.value)}
                          onBlur={commitEdit}
                          onKeyDown={(e) => {
                            if (e.key === 'Enter') {
                              e.preventDefault()
                              commitEdit()
                            }
                            if (e.key === 'Escape') {
                              // Ours: cancel the edit, keep the drawer open.
                              e.preventDefault()
                              setEditing(null)
                            }
                          }}
                          autoFocus
                          aria-label={`New value for ${key}`}
                          className="w-full min-w-0 bg-raised/30 px-3 py-2.5 font-mono text-sm outline-none ring-1 ring-inset ring-accent-strong"
                        />
                      ) : (
                        <button
                          type="button"
                          onClick={() => startEdit(key)}
                          title="Click to edit"
                          aria-label={`Edit ${key}`}
                          className="flex h-full w-full min-w-0 items-center px-3 py-2 text-left focus-visible:outline-2 focus-visible:-outline-offset-2 focus-visible:outline-accent-strong"
                        >
                          <span className="min-w-0 truncate font-mono text-sm text-ink-soft">
                            {revealAll ? (
                              env[key] || <span className="font-sans text-xs italic text-muted">empty</span>
                            ) : (
                              <span className="tracking-wider text-muted">••••••••</span>
                            )}
                          </span>
                        </button>
                      )}
                    </div>
                    <div
                      className="flex items-center justify-end gap-0.5 px-1.5"
                      role="group"
                      aria-label={`Actions for ${key}`}
                    >
                      <button
                        type="button"
                        onClick={() => copyValue(key)}
                        title="Copy value"
                        aria-label={`Copy the value of ${key}`}
                        className={ROW_ACTION}
                      >
                        {copied === key ? (
                          <Check className="size-3.5 text-success" aria-hidden />
                        ) : (
                          <ClipboardCopy className="size-3.5" aria-hidden />
                        )}
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
                    </div>
                  </div>
                ))}
              </div>
            )}

            {pasteMode ? (
              <div className="border-t border-line/70 first:border-t-0">
                <textarea
                  value={pasteText}
                  onChange={(e) => setPasteText(e.target.value)}
                  placeholder={'DATABASE_URL=postgres://…\nSMTP_HOST=mail.example.com'}
                  rows={4}
                  aria-label="Paste KEY=VALUE lines"
                  className="block w-full resize-y bg-transparent px-3 py-2.5 font-mono text-sm placeholder:text-muted/60 focus:bg-raised/30 focus:outline-none focus:ring-1 focus:ring-inset focus:ring-accent-strong"
                />
                <div className="flex flex-wrap items-center justify-between gap-2 border-t border-line/60 px-3 py-2">
                  <p className="text-xs text-muted">One KEY=VALUE per line; # comments are skipped.</p>
                  <Button kind="quiet" size="sm" onClick={addPasted} disabled={pasteText.trim() === ''}>
                    Add these
                  </Button>
                </div>
              </div>
            ) : (
              <form
                onSubmit={addVar}
                className="grid grid-cols-[minmax(0,1fr)_minmax(0,1.6fr)_4.5rem] items-stretch border-t border-line/70 first:border-t-0"
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
                <div className="flex items-center justify-end px-1.5">
                  <button
                    type="submit"
                    disabled={newKey.trim() === ''}
                    title="Add variable"
                    aria-label="Add variable"
                    className={`${ROW_ACTION} text-ink-soft disabled:pointer-events-none disabled:opacity-35`}
                  >
                    <Plus className="size-4" aria-hidden />
                  </button>
                </div>
              </form>
            )}
          </div>

          <div className="flex flex-wrap items-center justify-between gap-2 pt-1">
            <p className={`text-xs ${dirty ? 'text-ink-soft' : 'text-muted'}`}>
              {dirty ? 'Unsaved changes' : 'Click a value to edit it. Changes apply when you save.'}
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
          <SuccessNote message={note} />
        </>
      )}
      <ErrorNote message={error} />
    </section>
  )
}

import { useCallback, useEffect, useRef, useState } from 'react'
import { Dialog, DialogBackdrop, DialogPanel, DialogTitle } from '@headlessui/react'
import { KeyRound, Plus, RefreshCw, Search, Trash2, X } from 'lucide-react'
import { api, type RedisKey, type RedisValue } from './api'
import { Button, ErrorNote, IconButton, Spinner, errText } from './ui'

const inputStyle = 'w-full rounded border border-line bg-surface px-2 py-1.5 font-mono text-xs focus:outline-2 focus:outline-accent-strong'

export function RedisDialog({ space, service, onClose, onDelete }: {
  space: string
  service: string
  onClose: () => void
  onDelete: () => void
}) {
  const [keys, setKeys] = useState<RedisKey[]>([])
  const [cursor, setCursor] = useState('0')
  const [db, setDb] = useState<number | null>(null)
  const [search, setSearch] = useState('*')
  const [pattern, setPattern] = useState('*')
  const [selected, setSelected] = useState<RedisKey | null>(null)
  const [creating, setCreating] = useState(false)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')
  const requests = useRef(0)

  const scan = useCallback(async (next = '0') => {
    const request = ++requests.current
    setLoading(true)
    setError('')
    try {
      const result = await api.redisKeys(space, service, pattern, next)
      if (request !== requests.current) return
      setKeys(current => Array.from(new Map([...(next === '0' ? [] : current), ...result.keys].map(key => [key.id, key])).values()))
      setCursor(result.cursor)
      setDb(result.db)
    } catch (err) {
      if (request === requests.current) setError(errText(err, 'Redis did not answer.'))
    } finally {
      if (request === requests.current) setLoading(false)
    }
  }, [space, service, pattern])

  useEffect(() => {
    void scan()
    const cancel = () => { requests.current++ }
    return cancel
  }, [scan])

  return (
    <Dialog open onClose={onClose} className="relative z-50">
      <DialogBackdrop transition className="fixed inset-0 bg-black/60 transition-opacity duration-(--duration-base) data-closed:opacity-0" />
      <div className="fixed inset-0 flex items-center justify-center p-4">
        <DialogPanel transition className="flex h-[85vh] w-full max-w-5xl flex-col rounded-lg border border-line bg-card shadow-2xl transition duration-(--duration-base) data-closed:opacity-0 data-closed:scale-95">
          <div className="flex flex-wrap items-center justify-between gap-3 border-b border-line px-4 py-3">
            <DialogTitle className="flex items-center gap-2 font-mono text-[11px] font-semibold uppercase tracking-[0.14em] text-muted">
              <KeyRound className="size-3.5" aria-hidden /> {service} · {space}
              {db !== null && <span className="font-normal">· Database {db}</span>}
            </DialogTitle>
            <div className="flex items-center gap-1">
              <IconButton icon={RefreshCw} label="Refresh keys" disabled={loading} onClick={() => void scan()} />
              <IconButton icon={Plus} label="New key" onClick={() => { setCreating(true); setSelected(null) }} />
              <IconButton icon={Trash2} label="Delete service" kind="danger" onClick={onDelete} />
              <IconButton icon={X} label="Close Redis browser" onClick={onClose} />
            </div>
          </div>
          {error && <div className="px-4 pt-3"><ErrorNote message={error} /></div>}
          <div className="flex min-h-0 flex-1">
            <aside className="flex w-1/3 min-w-40 max-w-80 flex-col border-r border-line">
              <form className="flex items-center gap-1 border-b border-line p-3" onSubmit={event => {
                event.preventDefault()
                if (pattern === search) void scan()
                else setPattern(search)
              }}>
                <input className={inputStyle} aria-label="Key pattern" placeholder="session:*" value={search} onChange={event => setSearch(event.target.value)} />
                <IconButton type="submit" icon={Search} label="Find keys" disabled={loading} />
              </form>
              <div className="flex-1 overflow-auto p-2">
                {keys.map(key => <button key={key.id} type="button" onClick={() => { setSelected(key); setCreating(false) }} title={key.name}
                  className={`block w-full truncate rounded px-2 py-2 text-left font-mono text-xs ${selected?.id === key.id ? 'bg-accent-soft text-ink' : 'text-ink-soft hover:bg-line/40'}`}>
                  {key.name || '(empty key)'}
                </button>)}
                {loading && <Spinner label="Reading keys…" />}
                {!loading && keys.length === 0 && <p className="p-2 text-xs text-muted">{cursor === '0' ? 'No matching keys.' : 'No matches in this batch. Load more to keep searching.'}</p>}
              </div>
              {cursor !== '0' && <div className="border-t border-line p-3"><Button kind="quiet" disabled={loading} onClick={() => void scan(cursor)}>Load more keys</Button></div>}
              <p className="border-t border-line p-3 text-xs text-muted">{keys.length} keys loaded · Use * to match any text.</p>
            </aside>
            <main className="min-w-0 flex-1 overflow-auto p-4">
              {creating ? <NewKey space={space} service={service} onCreated={() => { setCreating(false); void scan() }} />
                : selected ? <KeyValue key={selected.id} space={space} service={service} entry={selected} onDeleted={() => { setSelected(null); void scan() }} />
                  : <p className="py-12 text-center text-sm text-muted">Choose a key to inspect its value and expiry.</p>}
            </main>
          </div>
        </DialogPanel>
      </div>
    </Dialog>
  )
}

function Expiry({ value, setValue, creating = false }: { value: string; setValue: (value: string) => void; creating?: boolean }) {
  return <label className="block text-xs text-muted">Expiry in seconds
    <input type="number" min="0" max="2147483647" step="1" aria-label="Expiry in seconds" className={`${inputStyle} mt-1 max-w-64`} placeholder={creating ? 'No expiry' : 'Keep current expiry'} value={value} onChange={event => setValue(event.target.value)} />
    <span className="ml-2">0 means no expiry.</span>
  </label>
}

function NewKey({ space, service, onCreated }: { space: string; service: string; onCreated: () => void }) {
  const [name, setName] = useState('')
  const [value, setValue] = useState('')
  const [ttl, setTTL] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  return <form className="space-y-4" onSubmit={async event => {
    event.preventDefault()
    setBusy(true); setError('')
    try {
      await api.redisCreate(space, service, name, value, ttl === '' ? undefined : Number(ttl))
      onCreated()
    } catch (err) { setError(errText(err, 'Creating the key failed.')) }
    finally { setBusy(false) }
  }}>
    <h3 className="text-sm font-semibold">New string key</h3>
    {error && <ErrorNote message={error} />}
    <label className="block text-xs text-muted">Name<input aria-label="Key name" className={`${inputStyle} mt-1`} maxLength={4096} value={name} onChange={event => setName(event.target.value)} /></label>
    <label className="block text-xs text-muted">Value<textarea aria-label="Key value" className={`${inputStyle} mt-1 min-h-48`} value={value} onChange={event => setValue(event.target.value)} /></label>
    <Expiry value={ttl} setValue={setTTL} creating />
    <Button type="submit" disabled={busy}>{busy ? 'Creating…' : 'Create key'}</Button>
  </form>
}

function KeyValue({ space, service, entry, onDeleted }: { space: string; service: string; entry: RedisKey; onDeleted: () => void }) {
  const [data, setData] = useState<RedisValue | null>(null)
  const [value, setValue] = useState('')
  const [ttl, setTTL] = useState('')
  const [refresh, setRefresh] = useState(0)
  const [busy, setBusy] = useState(false)
  const [loading, setLoading] = useState(true)
  const [deleting, setDeleting] = useState(false)
  const [error, setError] = useState('')
  useEffect(() => {
    let active = true
    setLoading(true)
    api.redisValue(space, service, entry.id).then(result => {
      if (!active) return
      setData(result); setValue(typeof result.value === 'string' ? result.value : '')
      setTTL(''); setError('')
    }).catch(err => { if (active) { setData(null); setError(errText(err, 'Reading the key failed.')) } })
      .finally(() => { if (active) setLoading(false) })
    return () => { active = false }
  }, [space, service, entry.id, refresh])
  return <div className="space-y-4">
    <div className="flex items-center justify-between gap-2">
      <h3 className="break-all font-mono text-sm font-semibold">{entry.name || '(empty key)'}</h3>
      <div className="flex gap-1">
        <IconButton icon={RefreshCw} label="Reload value" disabled={busy || loading} onClick={() => setRefresh(n => n + 1)} />
        <IconButton icon={Trash2} label="Delete key" kind="danger" disabled={busy} onClick={() => setDeleting(true)} />
      </div>
    </div>
    {error && <ErrorNote message={error} />}
    {deleting && <div className="flex flex-wrap items-center gap-3 rounded border border-danger/40 p-3 text-xs">
      <span>Delete this key?</span>
      <Button kind="danger" disabled={busy} onClick={async () => {
        setBusy(true); setError('')
        try { await api.redisDelete(space, service, entry.id); onDeleted() }
        catch (err) { setError(errText(err, 'Deleting the key failed.')) }
        finally { setBusy(false) }
      }}>Delete this key</Button>
      <IconButton icon={X} label="Keep key" disabled={busy} onClick={() => setDeleting(false)} />
    </div>}
    {loading ? <Spinner label="Reading value…" /> : data && <>
      <p className="font-mono text-xs text-muted">{data.type} · {data.size} {data.type === 'string' ? 'bytes' : 'items'} · {data.ttl < 0 ? 'No expiry' : `Expires in ${data.ttl}s`}</p>
      {data.editable ? <form className="space-y-4" onSubmit={async event => {
        event.preventDefault()
        setBusy(true); setError('')
        try {
          await api.redisSave(space, service, entry.id, value, data.value as string, ttl === '' ? undefined : Number(ttl))
          setRefresh(n => n + 1)
        } catch (err) { setError(errText(err, 'Saving the key failed.')) }
        finally { setBusy(false) }
      }}>
        <label className="block text-xs text-muted">Value<textarea aria-label="Key value" className={`${inputStyle} mt-1 min-h-64`} value={value} onChange={event => setValue(event.target.value)} /></label>
        <Expiry value={ttl} setValue={setTTL} />
        <Button type="submit" disabled={busy}>{busy ? 'Saving…' : 'Save value'}</Button>
      </form> : <>
        <pre className="overflow-auto whitespace-pre-wrap break-all rounded border border-line bg-surface p-3 font-mono text-xs text-ink-soft">{typeof data.value === 'string' ? data.value : JSON.stringify(data.value, null, 2)}</pre>
        <p className="text-xs text-muted">Read-only preview. Binary values are shown as base64.</p>
      </>}
      {data.truncated && <p className="text-xs text-muted">Preview limited to 100 items or 256 KB.</p>}
    </>}
  </div>
}

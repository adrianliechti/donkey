import { useState } from 'react'
import { Dialog, DialogBackdrop, DialogPanel, DialogTitle } from '@headlessui/react'
import { Search, Trash2, X } from 'lucide-react'
import { api } from './api'
import { Button, ErrorNote, IconButton, Spinner, errText } from './ui'

export function ExaDialog({ space, service, onClose, onDelete }: {
  space: string
  service: string
  onClose: () => void
  onDelete: () => void
}) {
  const [mode, setMode] = useState<'search' | 'contents'>('search')
  const [input, setInput] = useState('')
  const [loading, setLoading] = useState(false)
  const [results, setResults] = useState<{ title?: string; url: string; text?: string }[] | null>(null)
  const [error, setError] = useState('')

  async function submit() {
    setLoading(true)
    setError('')
    setResults(null)
    try {
      const body = mode === 'search'
        ? { query: input.trim(), numResults: 5, contents: { text: true } }
        : { ids: input.split(/\s+/).filter(Boolean), text: true }
      const result = await api.serviceExa(space, service, mode, body)
      setResults(result.results ?? [])
    } catch (err) {
      setError(errText(err, 'Search did not answer.'))
    } finally {
      setLoading(false)
    }
  }

  return (
    <Dialog open onClose={onClose} className="relative z-50">
      <DialogBackdrop className="fixed inset-0 bg-black/60" />
      <div className="fixed inset-0 flex items-center justify-center p-4">
        <DialogPanel className="flex h-[85vh] w-full max-w-5xl flex-col rounded-lg border border-line bg-card shadow-2xl">
          <div className="flex items-center justify-between gap-3 border-b border-line px-4 py-3">
            <DialogTitle className="flex items-center gap-2 font-mono text-[11px] font-semibold uppercase tracking-[0.14em] text-muted">
              <Search className="size-3.5" aria-hidden /> {service} · {space}
            </DialogTitle>
            <div className="flex gap-1">
              <IconButton icon={Trash2} label="Delete service" kind="danger" onClick={onDelete} />
              <IconButton icon={X} label="Close search browser" onClick={onClose} />
            </div>
          </div>
          <form className="flex flex-col gap-3 border-b border-line p-4" onSubmit={event => { event.preventDefault(); void submit() }}>
            <div className="flex gap-2">
              <Button kind={mode === 'search' ? 'primary' : 'quiet'} disabled={loading} onClick={() => { setMode('search'); setInput(''); setResults(null) }}>Search</Button>
              <Button kind={mode === 'contents' ? 'primary' : 'quiet'} disabled={loading} onClick={() => { setMode('contents'); setInput(''); setResults(null) }}>Page contents</Button>
            </div>
            <label className="text-sm text-ink-soft" htmlFor="exa-query">{mode === 'search' ? 'Search query' : 'Page URLs (one per line)'}</label>
            <textarea id="exa-query" className="w-full rounded border border-line bg-surface px-3 py-2 text-sm focus:outline-2 focus:outline-accent-strong"
              value={input} onChange={event => setInput(event.target.value)} rows={3} disabled={loading} />
            <div><Button type="submit" disabled={loading || !input.trim()}>{mode === 'search' ? 'Search the web' : 'Fetch contents'}</Button></div>
          </form>
          <div className="min-h-0 flex-1 overflow-auto p-4">
            {error && <ErrorNote message={error} />}
            {loading && <Spinner label={mode === 'search' ? 'Searching…' : 'Fetching page contents…'} />}
            {results?.length === 0 && <p className="text-sm text-muted">No results.</p>}
            {results?.map((result, index) => {
              const safeURL = /^https?:\/\//i.test(result.url)
              return <article key={`${index}:${result.url}`} className="mb-4 rounded border border-line p-4">
                <h3 className="font-semibold text-ink">{result.title || result.url}</h3>
                {safeURL && <a href={result.url} target="_blank" rel="noopener noreferrer" className="break-all text-sm text-accent-strong underline">{result.url}</a>}
                {result.text && <p className="mt-3 whitespace-pre-wrap text-sm text-ink-soft">{result.text}</p>}
              </article>
            })}
          </div>
        </DialogPanel>
      </div>
    </Dialog>
  )
}

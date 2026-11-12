import { useRef, useState, type KeyboardEvent } from 'react'
import { useQuery } from '@tanstack/react-query'
import { ExternalLink, FileText, Link2, RefreshCw, Search, SearchX, SlidersHorizontal } from 'lucide-react'
import type { ConsoleProps } from './consoles'
import {
  categories,
  contentsQuery,
  defaultCount,
  hostOf,
  optionsFrom,
  resultCounts,
  safeLink,
  searchKeys,
  searchQuery,
  searchTypes,
  type ExaResult,
  type SearchOptions,
} from './exaApi'
import { Markdown, MarkdownStyle } from './AiMarkdown'
import { CopyAs, Keys, Pane, PaneEmpty, PaneHeader, Picker, Segmented } from './Workbench'
import { useServiceSearch } from './serviceSearch'
import { Button, CopyButton, ErrorNote, Field, IconButton, Input, LinkButton, Select, Spinner } from './ui'
import { errText } from './format'

// The Search tab of an Exa-compatible service: a quick tester — a query and
// its options on top, the results on the left, the opened page on the
// right. The search and the open result are the console's place
// (?q=…&open=…), so a search can be linked and back walks through them —
// without paying for it twice (see searchQuery).

export function Console({ space, service }: ConsoleProps) {
  const [search, move] = useServiceSearch()
  const options = optionsFrom(search)
  const openURL = search.open ?? ''
  const results = useQuery(searchQuery(space, service, options))
  const [form, setForm] = useState(options)
  const [filters, setFilters] = useState(
    Boolean(options.cat || options.inc || options.exc || options.from || options.to || options.n),
  )
  // The form follows the place: back and forward, and a recent search.
  const placeKey = searchKeys.map((k) => options[k]).join('\u0000')
  const [shownKey, setShownKey] = useState(placeKey)
  if (shownKey !== placeKey) {
    setShownKey(placeKey)
    setForm(options)
  }

  const go = (o: SearchOptions, again = false) => {
    if (!o.q.trim()) return
    const next: Record<string, string> = {}
    for (const key of searchKeys) next[key] = key === 'q' ? o.q.trim() : o[key]
    // The search already shown is no new place; submitted again, it is
    // searched again.
    if (searchKeys.every((k) => next[k] === options[k])) {
      if (again) void results.refetch()
      return
    }
    // The tab's other keys (none today) stay; the open page does not.
    const rest = Object.fromEntries(
      Object.entries(search).filter(([k]) => !(searchKeys as readonly string[]).includes(k) && k !== 'open'),
    )
    move({ ...rest, ...next })
  }
  const openResult = (url: string) => {
    if (url !== openURL) move({ ...search, open: url }, openURL ? 'replace' : 'push')
  }

  const onKeyDown = (e: KeyboardEvent) => {
    if ((e.metaKey || e.ctrlKey) && e.key === 'Enter') {
      e.preventDefault()
      go(form, true)
    }
  }

  const found = results.data?.results ?? []
  const picked = found.find((r) => r.url === openURL)

  return (
    <div className="flex min-h-0 flex-1 divide-line md:divide-x" onKeyDown={onKeyDown}>
      <MarkdownStyle />
      <div className="flex min-w-0 flex-1 flex-col">
        <form
          className="flex shrink-0 flex-col border-b border-line"
          onSubmit={(e) => {
            e.preventDefault()
            go(form, true)
          }}
        >
          <div className="flex min-h-13 items-center gap-2 pr-2.5 pl-4">
            <Search className="size-4 shrink-0 text-muted" aria-hidden />
            <input
              aria-label="Search the web"
              value={form.q}
              onChange={(e) => setForm({ ...form, q: e.target.value })}
              placeholder="What are you looking for?"
              autoFocus={!options.q}
              className="h-10 min-w-0 flex-1 bg-transparent text-base text-ink outline-none placeholder:text-muted"
            />
            <Picker
              plain
              label="Search type"
              value={form.type}
              onChange={(type) => setForm({ ...form, type })}
              options={searchTypes}
            />
            <IconButton
              icon={SlidersHorizontal}
              label={filters ? 'Hide the filters' : 'Filters: results, kind, domains, dates'}
              active={filters}
              onClick={() => setFilters(!filters)}
            />
            <Button type="submit" icon={Search} disabled={!form.q.trim()}>
              Search
            </Button>
          </div>
          {filters && <Filters form={form} onChange={setForm} />}
        </form>

        <div className="grid min-h-0 flex-1 grid-rows-2 divide-y divide-line lg:divide-x lg:divide-y-0 lg:grid-cols-[minmax(16rem,2fr)_minmax(0,3fr)] lg:grid-rows-1 2xl:grid-cols-[minmax(20rem,28rem)_minmax(0,1fr)]">
          <Pane label="Results">
            <PaneHeader
              actions={
                results.data && (
                  <IconButton
                    icon={RefreshCw}
                    size="sm"
                    label="Search again"
                    disabled={results.isFetching}
                    className={results.isFetching ? '[&>svg]:animate-spin' : ''}
                    onClick={() => void results.refetch()}
                  />
                )
              }
            >
              <span className="flex min-w-0 items-center gap-2 text-xs text-muted">
                <span className="font-medium text-ink">
                  {results.data ? `${found.length} ${found.length === 1 ? 'result' : 'results'}` : 'Results'}
                </span>
                {results.data?.resolvedType && <span className="font-mono">{results.data.resolvedType}</span>}
                {results.data?.cost !== undefined && (
                  <span className="font-mono" title="What this search cost, as Exa reports it">
                    ${results.data.cost.toFixed(4)}
                  </span>
                )}
              </span>
            </PaneHeader>
            <Results
              options={options}
              loading={results.isFetching && !results.data}
              error={results.isError ? errText(results.error, 'The search did not work.') : ''}
              found={results.data ? found : undefined}
              openURL={openURL}
              onOpen={openResult}
            />
          </Pane>
          <Reader space={space} service={service} url={openURL} result={picked} />
        </div>
      </div>
    </div>
  )
}

function Filters({ form, onChange }: { form: SearchOptions; onChange: (o: SearchOptions) => void }) {
  const set = (patch: Partial<SearchOptions>) => onChange({ ...form, ...patch })
  const any = form.cat || form.inc || form.exc || form.from || form.to || form.n
  return (
    <div className="grid grid-cols-2 items-end gap-3 border-t border-line px-4 py-3 md:grid-cols-3 xl:grid-cols-6">
      <Field label="Results">
        <Select
          ariaLabel="Number of results"
          value={form.n || String(defaultCount)}
          onChange={(n) => set({ n: n === String(defaultCount) ? '' : n })}
          options={resultCounts.map((n) => ({ value: String(n), label: String(n) }))}
        />
      </Field>
      <Field label="Kind">
        <Select ariaLabel="Kind of page" value={form.cat} onChange={(cat) => set({ cat })} options={categories} />
      </Field>
      <Field label="Only from">
        <Input
          value={form.inc}
          onChange={(e) => set({ inc: e.target.value })}
          placeholder="example.com"
          spellCheck={false}
        />
      </Field>
      <Field label="Never from">
        <Input
          value={form.exc}
          onChange={(e) => set({ exc: e.target.value })}
          placeholder="example.org"
          spellCheck={false}
        />
      </Field>
      <Field label="Published after">
        <Input
          type="date"
          value={form.from}
          max={form.to || undefined}
          onChange={(e) => set({ from: e.target.value })}
        />
      </Field>
      <Field label="Published before">
        <Input type="date" value={form.to} min={form.from || undefined} onChange={(e) => set({ to: e.target.value })} />
      </Field>
      {any && (
        <div className="col-span-full -mt-1 flex justify-end">
          <Button
            kind="quiet"
            size="sm"
            onClick={() => onChange({ ...form, cat: '', inc: '', exc: '', from: '', to: '', n: '' })}
          >
            Clear the filters
          </Button>
        </div>
      )}
    </div>
  )
}

function Results({
  options,
  loading,
  error,
  found,
  openURL,
  onOpen,
}: {
  options: SearchOptions
  loading: boolean
  error: string
  found?: ExaResult[]
  openURL: string
  onOpen: (url: string) => void
}) {
  const list = useRef<HTMLUListElement>(null)
  // Up and down walk the results, opening each, as in a mail client.
  const step = (e: KeyboardEvent, by: number) => {
    if (!found?.length) return
    e.preventDefault()
    const at = found.findIndex((r) => r.url === openURL)
    const next = found[Math.min(found.length - 1, Math.max(0, at < 0 ? 0 : at + by))]
    onOpen(next.url)
    list.current?.querySelector<HTMLElement>(`[data-url="${CSS.escape(next.url)}"]`)?.focus()
  }

  if (!options.q) {
    return (
      <PaneEmpty icon={Search} title="Search the web">
        Results come from the service's Exa endpoint, with its key. <Keys chord="send" /> or Enter searches.
      </PaneEmpty>
    )
  }
  if (loading) return <Spinner label="Searching…" />
  if (error) {
    return (
      <div className="p-3">
        <ErrorNote message={error} />
      </div>
    )
  }
  if (found && found.length === 0) {
    return (
      <PaneEmpty icon={SearchX} title="Nothing found">
        Try other words, another search type, or fewer filters.
      </PaneEmpty>
    )
  }
  return (
    <ul
      ref={list}
      aria-label="Search results"
      className="min-h-0 flex-1 divide-y divide-line/60 overflow-y-auto"
      onKeyDown={(e) => {
        if (e.key === 'ArrowDown' || e.key === 'j') step(e, 1)
        if (e.key === 'ArrowUp' || e.key === 'k') step(e, -1)
      }}
    >
      {found?.map((r) => {
        const selected = r.url === openURL
        return (
          <li key={r.id}>
            <button
              type="button"
              data-url={r.url}
              onClick={() => onOpen(r.url)}
              aria-current={selected || undefined}
              className={`block w-full border-l-2 px-3 py-2.5 text-left transition-colors focus-visible:outline-2 focus-visible:-outline-offset-2 focus-visible:outline-accent-strong ${
                selected ? 'border-accent bg-accent-soft/50' : 'border-transparent hover:bg-line/40'
              }`}
            >
              <span className="line-clamp-2 text-sm font-medium text-ink">{r.title || r.url}</span>
              <span className="mt-0.5 block truncate font-mono text-[11px] text-muted">
                {hostOf(r.url)}
                {r.publishedDate && <> · {r.publishedDate.slice(0, 10)}</>}
                {r.author && <> · {r.author}</>}
              </span>
              {(r.highlights[0] || r.summary) && (
                <span className="mt-1 line-clamp-3 text-xs leading-relaxed text-ink-soft">
                  {r.highlights[0] || r.summary}
                </span>
              )}
            </button>
          </li>
        )
      })}
    </ul>
  )
}

function Reader({ space, service, url, result }: { space: string; service: string; url: string; result?: ExaResult }) {
  const page = useQuery(contentsQuery(space, service, url))
  const [plain, setPlain] = useState(false)
  const title = page.data?.title || result?.title || url
  const author = page.data?.author || result?.author
  const published = (page.data?.publishedDate || result?.publishedDate || '').slice(0, 10)
  const text = page.data?.text ?? ''

  if (!url) {
    return (
      <Pane label="Page">
        <PaneEmpty icon={FileText} title="Nothing open">
          Open a result to read the page here — its text, as Exa read it. ↑ and ↓ step through the results.
        </PaneEmpty>
      </Pane>
    )
  }

  return (
    <Pane label="Page">
      <PaneHeader
        actions={
          <>
            {text && (
              <Segmented
                label="Text view"
                value={plain ? 'plain' : 'formatted'}
                onChange={(v) => setPlain(v === 'plain')}
                options={[
                  { value: 'formatted', label: 'Formatted' },
                  { value: 'plain', label: 'Plain' },
                ]}
              />
            )}
            {text && <CopyButton text={text} label="Copy the text" />}
            <CopyAs icon={Link2} text={url} label="Copy the URL" />
            {safeLink(url) && (
              <LinkButton href={url} kind="quiet" size="sm" icon={ExternalLink}>
                Open
              </LinkButton>
            )}
          </>
        }
      >
        <span className="min-w-0 truncate text-sm font-medium text-ink" title={title}>
          {title}
        </span>
      </PaneHeader>
      <div className="min-h-0 flex-1 overflow-y-auto">
        <article className="mx-auto max-w-3xl space-y-4 px-5 py-4">
          <header className="space-y-1">
            {safeLink(url) ? (
              <a
                href={url}
                target="_blank"
                rel="noopener noreferrer"
                className="block font-mono text-xs break-all text-accent-strong hover:underline"
              >
                {url}
              </a>
            ) : (
              <p className="font-mono text-xs break-all text-muted">{url}</p>
            )}
            {(author || published) && (
              <p className="text-xs text-muted">
                {[author, published && `published ${published}`].filter(Boolean).join(' · ')}
              </p>
            )}
          </header>
          {result && result.highlights.length > 0 && (
            <section aria-label="Highlights" className="space-y-1.5 rounded-lg bg-accent-soft/40 px-3 py-2.5">
              {result.highlights.map((h, i) => (
                <p key={i} className="text-xs leading-relaxed text-ink-soft">
                  {h}
                </p>
              ))}
            </section>
          )}
          {page.isPending && <Spinner label="Reading the page…" />}
          {page.isError && <ErrorNote message={errText(page.error, 'The page could not be read.')} />}
          {page.isSuccess && !text && <p className="text-sm text-muted">Exa has no text for this page.</p>}
          {text &&
            (plain ? (
              <pre className="font-sans text-sm leading-relaxed break-words whitespace-pre-wrap text-ink-soft">
                {text}
              </pre>
            ) : (
              <Markdown text={text} className="text-sm leading-relaxed text-ink-soft" />
            ))}
        </article>
      </div>
    </Pane>
  )
}

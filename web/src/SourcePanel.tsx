import { useEffect, useMemo, useRef, useState } from 'react'
import { useNavigate, useSearch } from '@tanstack/react-router'
import {
  ChevronLeft,
  ChevronRight,
  Download,
  File,
  FileCode2,
  Folder,
  FolderOpen,
  FolderTree,
  GitCommitHorizontal,
  GitCompareArrows,
} from 'lucide-react'
import { api, type CommitChange, saveBlob, type SourceCommit, type SourceEntry } from './api'
import { DiffView } from './DiffView'
import { SourceCode } from './SourceCode'
import { useCommit, useCommitDiff, useCommitFile, useCommits, useCommitTree } from './queries'
import {
  Button,
  CopyButton,
  DonkeyLogo,
  ErrorNote,
  IconButton,
  List,
  ListRow,
  SectionHeader,
  Select,
  Spinner,
  Toolbar,
} from './ui'
import { ago, errText, humanSize } from './format'
import type { SourceSearch } from './sourceSearch'

// The Source tab: the app's history as git keeps it. Every deploy's upload
// is a commit by the person who deployed it, and when the build changed its
// copy — a generated Dockerfile, an automatic repair — the platform's own
// commit sits on top. Three views, one at a time: the history, one commit's
// changes, and the files of a version. Which one is open lives in the URL
// (see sourceSearch.ts), so back and forward walk the drilldown and a
// commit or a file can be linked.

// A shown file is bounded: beyond this the browser would be laying out a
// table for minutes. The download is always there.
const maxShownSize = 1 << 20

export function SourcePanel({ space, app }: { space: string; app: string }) {
  const search = useSearch({ strict: false }) as SourceSearch
  const navigate = useNavigate()
  const root = useRef<HTMLDivElement>(null)
  const opened = useRef(false)
  const key = `${search.commit ?? 'history'}:${search.files ?? ''}`
  // A different view opens at its top, like a different tab does.
  useEffect(() => {
    if (opened.current) root.current?.scrollIntoView({ block: 'start' })
    opened.current = true
  }, [key])

  const go = (next: SourceSearch) =>
    navigate({ to: '/app/$space/$name/{-$tab}', params: { space, name: app, tab: 'source' }, search: next, resetScroll: false })
  const open = (id: string) => go({ commit: id })
  const browse = (id: string, path: string | null = null) => go({ commit: id, files: 1, path: path ?? undefined })
  const history = () => go({})

  return (
    // scroll-mt clears the console's bar and the app's tab bar. Browsing
    // files is a workbench that fills the sheet (see AppDetail).
    <div ref={root} className={`flex scroll-mt-28 flex-col gap-5 ${search.files ? 'min-h-0 flex-1' : ''}`}>
      {!search.commit && <HistoryView space={space} app={app} onOpen={open} onBrowse={browse} />}
      {search.commit && !search.files && (
        // Keyed: following a parent link opens a fresh view, not this one's
        // open diffs and errors.
        <CommitView key={search.commit} space={space} app={app} id={search.commit} onHistory={history} onOpen={open} onBrowse={browse} />
      )}
      {search.commit && search.files && (
        <BrowseView
          space={space}
          app={app}
          id={search.commit}
          path={search.path ?? null}
          onHistory={history}
          onOpen={open}
          onBrowse={browse}
        />
      )}
    </div>
  )
}

// ─── History ────────────────────────────────────────────────────────────

function HistoryView({
  space,
  app,
  onOpen,
  onBrowse,
}: {
  space: string
  app: string
  onOpen: (id: string) => void
  onBrowse: (id: string) => void
}) {
  const { data, error } = useCommits(space, app)
  if (!data && !error) return <Spinner label="Opening the source history…" />
  if (error) return <ErrorNote message={error} />
  if (!data) return null
  if (data.commits.length === 0) {
    return (
      <section className="space-y-3">
        <SectionHeader title="Source" />
        <p className="text-sm text-muted">No source is recorded for this app yet — the next deploy starts its history.</p>
      </section>
    )
  }
  const current = data.commits.find((c) => c.live) ?? data.commits[0]

  return (
    <section className="space-y-3">
      <SectionHeader
        title="Source"
        description="Each deploy's files as a commit — yours against your previous upload, the platform's against the upload it built."
        actions={<IconButton icon={FolderTree} label="Browse the files that run now" onClick={() => onBrowse(current.id)} />}
      />
      <List>
        {data.commits.map((c) => (
          <CommitRow key={c.id} space={space} app={app} commit={c} onOpen={onOpen} onBrowse={onBrowse} />
        ))}
      </List>
    </section>
  )
}

function CommitRow({
  space,
  app,
  commit,
  onOpen,
  onBrowse,
}: {
  space: string
  app: string
  commit: SourceCommit
  onOpen: (id: string) => void
  onBrowse: (id: string) => void
}) {
  const download = useDownload()
  return (
    <ListRow
      className={commit.kind === 'platform' ? 'bg-raised/40' : ''}
      actions={
        <>
          <IconButton
            icon={FolderTree}
            size="sm"
            label="Browse the files of this version"
            onClick={() => onBrowse(commit.id)}
          />
          <IconButton
            icon={Download}
            size="sm"
            label={download.busy ? 'Downloading…' : 'Download this version as .zip'}
            disabled={download.busy}
            onClick={() => download.run(api.downloadArchive(space, app, commit.id), archiveName(app, commit.id))}
          />
        </>
      }
    >
      <Avatar commit={commit} />
      <button
        type="button"
        onClick={() => onOpen(commit.id)}
        className="min-w-0 flex-1 rounded text-left focus-visible:outline-2 focus-visible:outline-accent-strong"
      >
        <span className="flex min-w-0 items-center gap-2">
          <span className="truncate text-sm text-ink hover:underline">{subject(commit.message)}</span>
          {commit.live && <LiveChip />}
        </span>
        <span className="mt-0.5 flex flex-wrap items-center gap-x-2 gap-y-0.5 text-xs text-muted">
          <span className="text-ink-soft">{commit.author.name}</span>
          <span title={longDate(commit.date)}>{ago(commit.date)} ago</span>
          <span className="font-mono">{short(commit.id)}</span>
          <BuildChips builds={commit.builds} />
        </span>
        {download.error && <span className="mt-1 block text-xs text-danger">{download.error}</span>}
      </button>
      <ChangeStats files={commit.stats.files} additions={commit.stats.additions} deletions={commit.stats.deletions} />
    </ListRow>
  )
}

// ─── One commit ─────────────────────────────────────────────────────────

// The first few files open with their diff; the rest wait to be asked, so a
// commit of five hundred files does not fetch five hundred diffs.
const openDiffs = 8

function CommitView({
  space,
  app,
  id,
  onHistory,
  onOpen,
  onBrowse,
}: {
  space: string
  app: string
  id: string
  onHistory: () => void
  onOpen: (id: string) => void
  onBrowse: (id: string, path?: string | null) => void
}) {
  const { data: commit, error } = useCommit(space, app, id)
  const { data: history } = useCommits(space, app)
  const download = useDownload()

  // Stepping through the history without going back to it: newer is up the
  // list, older down — and j / k, as in most code browsers.
  const list = history?.commits ?? []
  const at = list.findIndex((c) => c.id === id)
  const newer = at > 0 ? list[at - 1] : undefined
  const older = at >= 0 && at < list.length - 1 ? list[at + 1] : undefined
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      const target = e.target as HTMLElement | null
      if (e.metaKey || e.ctrlKey || e.altKey || target?.closest('input, textarea, select, [contenteditable]')) return
      if (e.key === 'j' && older) onOpen(older.id)
      if (e.key === 'k' && newer) onOpen(newer.id)
    }
    document.addEventListener('keydown', onKey)
    return () => document.removeEventListener('keydown', onKey)
  }, [newer, older, onOpen])

  return (
    <section className="space-y-4">
      <Trail steps={[{ label: 'History', onClick: onHistory }, { label: commit ? `${short(id)} · ${subject(commit.message)}` : short(id) }]}>
        <Button kind="quiet" size="sm" icon={ChevronLeft} disabled={!newer} onClick={() => newer && onOpen(newer.id)} title="Newer commit (k)">
          Newer
        </Button>
        <Button kind="quiet" size="sm" disabled={!older} onClick={() => older && onOpen(older.id)} title="Older commit (j)">
          Older
          <ChevronRight className="size-3.5" aria-hidden />
        </Button>
        <span className="mx-0.5 h-4 w-px bg-line" aria-hidden />
        <IconButton icon={FolderTree} label="Browse the files of this version" onClick={() => onBrowse(id)} />
        <IconButton
          icon={Download}
          label={download.busy ? 'Downloading…' : 'Download this version as .zip'}
          disabled={download.busy}
          onClick={() => download.run(api.downloadArchive(space, app, id), archiveName(app, id))}
        />
      </Trail>
      <ErrorNote message={error || download.error} />
      {!commit && !error && <Spinner label="Opening the commit…" />}
      {commit && (
        <>
          <div className="flex items-start gap-3">
            <Avatar commit={commit} />
            <div className="min-w-0 flex-1">
              <h2 className="flex flex-wrap items-center gap-2 text-base font-semibold text-ink">
                {subject(commit.message)}
                {commit.live && <LiveChip />}
              </h2>
              <p className="mt-0.5 flex flex-wrap items-center gap-x-2 gap-y-0.5 text-xs text-muted">
                <span className="text-ink-soft">{commit.author.name}</span>
                {commit.author.email && <span>{commit.author.email}</span>}
                <span>{longDate(commit.date)}</span>
                <span className="flex items-center font-mono">
                  {short(commit.id)}
                  <CopyButton text={commit.id} label="Copy the commit id" />
                </span>
                <BuildChips builds={commit.builds} />
              </p>
              {body(commit.message) && (
                <p className="mt-2 whitespace-pre-wrap text-xs leading-relaxed text-ink-soft">{body(commit.message)}</p>
              )}
            </div>
          </div>

          <div className="flex flex-wrap items-center justify-between gap-2 text-xs text-muted">
            <span>
              {commit.parents.length === 0 ? (
                'The first recorded upload — every file is new.'
              ) : (
                <>
                  Compared with {commit.kind === 'platform' ? 'the upload it built' : 'the previous upload'},{' '}
                  <button
                    type="button"
                    onClick={() => onOpen(commit.parents[commit.parents.length - 1])}
                    className="rounded font-mono text-ink-soft underline-offset-2 hover:text-ink hover:underline focus-visible:outline-2 focus-visible:outline-accent-strong"
                  >
                    {short(commit.parents[commit.parents.length - 1])}
                  </button>
                </>
              )}
            </span>
            <ChangeStats files={commit.stats.files} additions={commit.stats.additions} deletions={commit.stats.deletions} long />
          </div>

          {commit.changes.length === 0 ? (
            <p className="text-sm text-muted">This commit changed no files.</p>
          ) : (
            <List>
              {commit.changes.map((change, i) => (
                <ChangeItem
                  key={change.path}
                  space={space}
                  app={app}
                  id={id}
                  change={change}
                  defaultOpen={i < openDiffs}
                  onView={() => onBrowse(id, change.path)}
                />
              ))}
            </List>
          )}
        </>
      )}
    </section>
  )
}

function ChangeItem({
  space,
  app,
  id,
  change,
  defaultOpen,
  onView,
}: {
  space: string
  app: string
  id: string
  change: CommitChange
  defaultOpen: boolean
  onView: () => void
}) {
  const diffable = !change.binary && !change.tooLarge
  const [open, setOpen] = useState(defaultOpen && diffable)
  const { data: diff, error } = useCommitDiff(space, app, id, change.path, open && diffable)

  return (
    <li>
      <div className="flex min-h-11 items-center gap-2 py-1.5 pl-3 pr-3">
        <button
          type="button"
          onClick={() => setOpen(!open)}
          aria-expanded={open}
          className="flex min-w-0 flex-1 items-center gap-2 rounded text-left focus-visible:outline-2 focus-visible:outline-accent-strong"
        >
          <ChevronRight className={`size-3.5 shrink-0 text-muted transition-transform ${open ? 'rotate-90' : ''}`} aria-hidden />
          <StatusMark status={change.status} />
          <span className="truncate font-mono text-xs text-ink">{change.path}</span>
        </button>
        {change.binary ? (
          <span className="shrink-0 text-xs text-muted">binary</span>
        ) : change.tooLarge ? (
          <span className="shrink-0 text-xs text-muted">too large to diff</span>
        ) : (
          <ChangeStats additions={change.additions} deletions={change.deletions} />
        )}
        {change.status !== 'deleted' && (
          <div className="-mr-1.5">
            <IconButton icon={FileCode2} size="sm" label="View the whole file at this version" onClick={onView} />
          </div>
        )}
      </div>
      {open && (
        <div className="px-3 pb-3">
          {!diffable ? (
            <p className="text-xs text-muted">
              {change.binary
                ? 'A binary file — download both versions to compare them.'
                : 'Too large to show as a diff here — download the versions to compare them.'}
            </p>
          ) : error ? (
            <ErrorNote message={error} />
          ) : !diff ? (
            <Spinner label="Reading the change…" inline />
          ) : diff.diff ? (
            <DiffView diff={diff.diff} />
          ) : (
            <p className="text-xs text-muted">Only the file's mode changed.</p>
          )}
        </div>
      )}
    </li>
  )
}

// ─── Browse a version ───────────────────────────────────────────────────

function BrowseView({
  space,
  app,
  id,
  path,
  onHistory,
  onOpen,
  onBrowse,
}: {
  space: string
  app: string
  id: string
  path: string | null
  onHistory: () => void
  onOpen: (id: string) => void
  onBrowse: (id: string, path?: string | null) => void
}) {
  const { data: history } = useCommits(space, app)
  const { data: tree, error } = useCommitTree(space, app, id)
  const { data: commit } = useCommit(space, app, id)
  const download = useDownload()

  const files = tree?.files
  const changed = useMemo(() => new Map((commit?.changes ?? []).map((c) => [c.path, c])), [commit])
  // Nothing picked yet: the README, if there is one, is what a person opens
  // first in any repository.
  const shown = useMemo(() => {
    if (path !== null || !files) return path
    const readme = files.find((f) => !f.path.includes('/') && /^readme(\.|$)/i.test(f.path))
    return readme?.path ?? null
  }, [path, files])

  const versions = (history?.commits ?? []).map((c) => ({
    value: c.id,
    label: `${short(c.id)} · ${subject(c.message).slice(0, 48)}${c.live ? ' · live' : ''}`,
  }))
  if (!versions.some((v) => v.value === id)) versions.unshift({ value: id, label: short(id) })

  const subjectLine = (history?.commits ?? []).find((c) => c.id === id)?.message
  return (
    // The tree and the file scroll on their own; the page stands still.
    <section className="flex min-h-0 flex-1 flex-col gap-3">
      <Trail
        steps={[
          { label: 'History', onClick: onHistory },
          { label: subjectLine ? `${short(id)} · ${subject(subjectLine)}` : short(id), onClick: () => onOpen(id) },
          { label: 'Files', onClick: shown ? () => onBrowse(id, null) : undefined },
          ...(shown ? [{ label: shown, mono: true }] : []),
        ]}
      >
        <Select
          variant="quiet"
          value={id}
          onChange={(next) => onBrowse(next, shown)}
          options={versions}
          ariaLabel="Version to browse"
        />
        <IconButton icon={GitCommitHorizontal} label="Show what this version changed" onClick={() => onOpen(id)} />
        <IconButton
          icon={Download}
          label={download.busy ? 'Downloading…' : 'Download this version as .zip'}
          disabled={download.busy}
          onClick={() => download.run(api.downloadArchive(space, app, id), archiveName(app, id))}
        />
      </Trail>
      <ErrorNote message={error || download.error} />
      {!files && !error && <Spinner label="Opening the files…" />}
      {files && (
        <div className="grid min-h-0 flex-1 grid-rows-[auto_minmax(0,1fr)] gap-3 md:grid-cols-[15rem_minmax(0,1fr)] md:grid-rows-[minmax(0,1fr)]">
          <FileTree files={files} selected={shown} changed={changed} onSelect={(p) => onBrowse(id, p)} />
          {shown ? (
            <FilePane
              key={`${id}:${shown}`}
              space={space}
              app={app}
              id={id}
              path={shown}
              change={changed.get(shown)}
              size={files.find((f) => f.path === shown)?.size}
            />
          ) : (
            <p className="py-6 text-center text-sm text-muted">Pick a file to read it as it was in this version.</p>
          )}
        </div>
      )}
    </section>
  )
}

interface Dir {
  name: string
  path: string
  dirs: Dir[]
  files: SourceEntry[]
}

function buildTree(files: SourceEntry[]): Dir {
  const root: Dir = { name: '', path: '', dirs: [], files: [] }
  const index = new Map<string, Dir>([['', root]])
  for (const file of files) {
    const parts = file.path.split('/')
    let at = root
    for (let i = 0; i < parts.length - 1; i++) {
      const p = parts.slice(0, i + 1).join('/')
      let next = index.get(p)
      if (!next) {
        next = { name: parts[i], path: p, dirs: [], files: [] }
        index.set(p, next)
        at.dirs.push(next)
      }
      at = next
    }
    at.files.push(file)
  }
  const sort = (d: Dir) => {
    d.dirs.sort((a, b) => a.name.localeCompare(b.name))
    d.files.sort((a, b) => a.path.localeCompare(b.path))
    d.dirs.forEach(sort)
  }
  sort(root)
  return root
}

function ancestors(path: string | null): string[] {
  if (!path) return []
  const parts = path.split('/')
  return parts.slice(0, -1).map((_, i) => parts.slice(0, i + 1).join('/'))
}

function FileTree({
  files,
  selected,
  changed,
  onSelect,
}: {
  files: SourceEntry[]
  selected: string | null
  changed: Map<string, CommitChange>
  onSelect: (path: string) => void
}) {
  const root = useMemo(() => buildTree(files), [files])
  const [expanded, setExpanded] = useState(() => new Set(ancestors(selected)))
  // A file picked from elsewhere (a commit's change list) opens its folders.
  useEffect(() => {
    setExpanded((prev) => {
      const missing = ancestors(selected).filter((p) => !prev.has(p))
      return missing.length === 0 ? prev : new Set([...prev, ...missing])
    })
  }, [selected])
  const toggle = (p: string) =>
    setExpanded((prev) => {
      const next = new Set(prev)
      if (next.has(p)) next.delete(p)
      else next.add(p)
      return next
    })

  const rows: React.ReactNode[] = []
  const walk = (dir: Dir, depth: number) => {
    for (const d of dir.dirs) {
      const open = expanded.has(d.path)
      rows.push(
        <TreeRow key={`d:${d.path}`} depth={depth} onClick={() => toggle(d.path)} expanded={open}>
          {open ? (
            <FolderOpen className="size-3.5 shrink-0 text-accent" aria-hidden />
          ) : (
            <Folder className="size-3.5 shrink-0 text-accent" aria-hidden />
          )}
          <span className="truncate">{d.name}</span>
        </TreeRow>,
      )
      if (open) walk(d, depth + 1)
    }
    for (const f of dir.files) {
      const change = changed.get(f.path)
      rows.push(
        <TreeRow key={`f:${f.path}`} depth={depth} onClick={() => onSelect(f.path)} selected={f.path === selected}>
          <File className="size-3.5 shrink-0 text-muted" aria-hidden />
          <span className="truncate">{f.path.split('/').pop()}</span>
          {change && (
            <span
              title={`${change.status} in this version`}
              className={`ml-auto size-1.5 shrink-0 rounded-full ${change.status === 'added' ? 'bg-success' : 'bg-warning'}`}
            />
          )}
        </TreeRow>,
      )
    }
  }
  walk(root, 0)

  return (
    <nav
      aria-label="Files of this version"
      className="max-h-64 overflow-auto rounded-lg border border-line py-1 md:max-h-none"
    >
      {rows}
    </nav>
  )
}

function TreeRow({
  depth,
  onClick,
  selected = false,
  expanded,
  children,
}: {
  depth: number
  onClick: () => void
  selected?: boolean
  expanded?: boolean
  children: React.ReactNode
}) {
  return (
    <button
      type="button"
      onClick={onClick}
      aria-expanded={expanded}
      aria-current={selected || undefined}
      style={{ paddingLeft: `${0.5 + depth * 0.875}rem` }}
      className={`flex h-7 w-full items-center gap-1.5 pr-2 text-left font-mono text-xs transition-colors focus-visible:outline-2 focus-visible:-outline-offset-2 focus-visible:outline-accent-strong ${
        selected ? 'bg-accent-soft text-ink' : 'text-ink-soft hover:bg-line/50 hover:text-ink'
      }`}
    >
      {children}
    </button>
  )
}

function FilePane({
  space,
  app,
  id,
  path,
  change,
  size,
}: {
  space: string
  app: string
  id: string
  path: string
  change?: CommitChange
  /** From the tree; undefined when the file is not in this version. */
  size?: number
}) {
  const exists = size !== undefined
  // Too large to show is known from the tree — no need to fetch it to say so.
  const showable = exists && size <= maxShownSize
  const { data: file, error } = useCommitFile(space, app, id, showable ? path : null)
  const diffable = !!change && !change.binary && !change.tooLarge
  const [showChanges, setShowChanges] = useState(false)
  const { data: diff, error: diffError } = useCommitDiff(space, app, id, path, showChanges && diffable)
  const download = useDownload()

  return (
    <div className="flex min-h-0 min-w-0 flex-col overflow-hidden rounded-lg border border-line">
      <div className="flex min-h-10 items-center justify-between gap-2 border-b border-line py-1 pl-3 pr-1.5">
        <span className="min-w-0 truncate font-mono text-xs text-ink" title={path}>
          {path}
        </span>
        <Toolbar>
          {exists && <span className="px-1 text-xs text-muted">{humanSize(size)}</span>}
          {diffable && (
            <IconButton
              icon={GitCompareArrows}
              size="sm"
              active={showChanges}
              label={showChanges ? 'Show the file' : 'Show what this version changed in the file'}
              onClick={() => setShowChanges(!showChanges)}
            />
          )}
          <IconButton
            icon={Download}
            size="sm"
            label={`Download ${path.split('/').pop()}`}
            disabled={!exists || download.busy}
            onClick={() => download.run(api.downloadCommitFile(space, app, id, path), path.split('/').pop() ?? 'file')}
          />
        </Toolbar>
      </div>
      <div className="min-h-0 flex-1 overflow-auto">
        <ErrorNote message={(showChanges ? diffError : error) || download.error} />
        {!exists ? (
          <p className="p-4 text-sm text-muted">This file is not part of this version.</p>
        ) : showChanges ? (
          !diff ? (
            !diffError && <Spinner label="Reading the change…" />
          ) : diff.diff ? (
            <div className="p-2">
              <DiffView diff={diff.diff} />
            </div>
          ) : (
            <p className="p-4 text-sm text-muted">Only the file's mode changed.</p>
          )
        ) : !showable ? (
          <p className="p-4 text-sm text-muted">Too large to show here — download it instead.</p>
        ) : !file ? (
          !error && <Spinner label="Opening the file…" />
        ) : file.binary ? (
          <p className="p-4 text-sm text-muted">A binary file — download it to open it.</p>
        ) : file.text === '' ? (
          <p className="p-4 text-sm text-muted">This file is empty.</p>
        ) : (
          <SourceCode path={path} text={file.text} className="" />
        )}
      </div>
    </div>
  )
}

// ─── Pieces ─────────────────────────────────────────────────────────────

// Where the drilldown stands — History / commit / Files / path — with every
// step but the last a way back to it, and the view's own tools on the right.
type Step = { label: string; onClick?: () => void; mono?: boolean }

function Trail({ steps, children }: { steps: Step[]; children: React.ReactNode }) {
  return (
    <div className="flex min-h-8 flex-wrap items-center justify-between gap-x-4 gap-y-2">
      <nav aria-label="Source" className="flex min-w-0 flex-wrap items-center gap-1 text-sm">
        {steps.map((step, i) => {
          const last = i === steps.length - 1
          return (
            <span key={i} className="flex min-w-0 items-center gap-1">
              {i > 0 && <ChevronRight className="size-3.5 shrink-0 text-muted" aria-hidden />}
              {step.onClick && !last ? (
                <button
                  type="button"
                  onClick={step.onClick}
                  className={`max-w-80 truncate rounded px-1 text-muted underline-offset-2 hover:text-ink hover:underline focus-visible:outline-2 focus-visible:outline-accent-strong ${step.mono ? 'font-mono' : ''}`}
                >
                  {step.label}
                </button>
              ) : (
                <span
                  aria-current={last ? 'page' : undefined}
                  className={`max-w-[32rem] truncate px-1 ${last ? 'font-medium text-ink' : 'text-muted'} ${step.mono ? 'font-mono text-[13px]' : ''}`}
                >
                  {step.label}
                </span>
              )}
            </span>
          )
        })}
      </nav>
      <Toolbar>{children}</Toolbar>
    </div>
  )
}

// The platform signs with the donkey; people with their initials.
function Avatar({ commit }: { commit: SourceCommit }) {
  if (commit.kind === 'platform') {
    return (
      <span
        title="Donkey Platform — what the build changed"
        className="flex size-7 shrink-0 items-center justify-center rounded-full bg-raised text-ink-soft"
      >
        <DonkeyLogo className="size-4" />
      </span>
    )
  }
  return (
    <span
      title={commit.author.email ? `${commit.author.name} <${commit.author.email}>` : commit.author.name}
      className="flex size-7 shrink-0 items-center justify-center rounded-full bg-accent-soft font-mono text-[11px] font-semibold text-accent"
    >
      {initials(commit.author.name)}
    </span>
  )
}

function LiveChip() {
  return (
    <span
      title="This is what the app runs right now"
      className="shrink-0 rounded bg-success/10 px-1.5 py-0.5 font-mono text-[10px] leading-4 text-success"
    >
      live
    </span>
  )
}

function BuildChips({ builds }: { builds?: SourceCommit['builds'] }) {
  if (!builds?.length) return null
  const shown = builds.slice(0, 3)
  return (
    <>
      {shown.map((b) => (
        <span
          key={b.sequence}
          title={b.ok === false ? `Build #${b.sequence} failed` : `Built as #${b.sequence}`}
          className={`rounded px-1 font-mono text-[10px] leading-4 ${
            b.ok === false ? 'bg-danger/10 text-danger' : 'bg-raised text-ink-soft'
          }`}
        >
          #{b.sequence}
          {b.ok === false ? ' failed' : ''}
        </span>
      ))}
      {builds.length > shown.length && <span className="font-mono text-[10px]">+{builds.length - shown.length}</span>}
    </>
  )
}

function ChangeStats({
  files,
  additions,
  deletions,
  long = false,
}: {
  files?: number
  additions: number
  deletions: number
  long?: boolean
}) {
  return (
    <span className="flex shrink-0 items-center gap-2 font-mono text-xs">
      {files !== undefined && (
        <span className={`text-muted ${long ? '' : 'hidden sm:inline'}`}>
          {files} {files === 1 ? 'file' : 'files'}
          {long ? ' changed' : ''}
        </span>
      )}
      <span className={additions ? 'text-success' : 'text-muted'}>+{additions}</span>
      <span className={deletions ? 'text-danger' : 'text-muted'}>−{deletions}</span>
    </span>
  )
}

const statusMark: Record<CommitChange['status'], { letter: string; tone: string }> = {
  added: { letter: 'A', tone: 'bg-success/10 text-success' },
  modified: { letter: 'M', tone: 'bg-warning/10 text-warning' },
  deleted: { letter: 'D', tone: 'bg-danger/10 text-danger' },
}

function StatusMark({ status }: { status: CommitChange['status'] }) {
  const mark = statusMark[status]
  return (
    <span
      title={status}
      className={`flex size-4 shrink-0 items-center justify-center rounded font-mono text-[10px] font-semibold ${mark.tone}`}
    >
      {mark.letter}
    </span>
  )
}

// useDownload saves a fetched blob under a name, with one busy/error state
// per place that offers the download.
function useDownload() {
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  return {
    busy,
    error,
    run: async (blob: Promise<Blob>, name: string) => {
      setBusy(true)
      setError('')
      try {
        saveBlob(await blob, name)
      } catch (err) {
        setError(errText(err, 'The download did not work.'))
      } finally {
        setBusy(false)
      }
    },
  }
}

const short = (id: string) => id.slice(0, 7)

const archiveName = (app: string, id: string) => `${app}-${id.slice(0, 12)}.zip`

function subject(message: string): string {
  return message.split('\n')[0]
}

function body(message: string): string {
  return message.split('\n').slice(1).join('\n').trim()
}

function initials(name: string): string {
  const words = name.replace(/[^\p{L}\p{N}\s]/gu, ' ').trim().split(/\s+/)
  return ((words[0]?.[0] ?? '?') + (words.length > 1 ? words[words.length - 1][0] : '')).toUpperCase()
}

function longDate(iso: string): string {
  const when = new Date(iso)
  return Number.isNaN(when.getTime()) ? iso : when.toLocaleString()
}

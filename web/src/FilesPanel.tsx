import { useCallback, useEffect, useRef, useState } from 'react'
import {
  Check,
  ChevronRight,
  Download,
  File,
  Folder,
  FolderPlus,
  HardDrive,
  Pencil,
  RefreshCw,
  Trash2,
  Upload,
  X,
} from 'lucide-react'
import { api, type FileListing } from './api'
import { Button, ErrorNote, errText, humanSize, IconButton, Input, List, ListRow, saveBlob, SectionHeader, Spinner } from './ui'

// FilesPanel browses the app's disk — the one part of an app a redeploy
// cannot rebuild, so seeding it and getting things out of it belong here.
export function FilesPanel({ space, app }: { space: string; app: string }) {
  const [path, setPath] = useState('')
  const [listing, setListing] = useState<FileListing | null>(null)
  const [error, setError] = useState('')
  const [busy, setBusy] = useState('')
  const [dragOver, setDragOver] = useState(false)
  const [noDisk, setNoDisk] = useState(false)
  // Renaming, deleting and new folders all happen inline: a browser prompt()
  // reads as somebody else's dialog in the middle of donkey's own page.
  const [renaming, setRenaming] = useState<{ name: string; value: string } | null>(null)
  const [deleting, setDeleting] = useState('')
  const [newFolder, setNewFolder] = useState<string | null>(null)
  const fileInput = useRef<HTMLInputElement>(null)

  const load = useCallback(async () => {
    try {
      setListing(await api.files(space, app, path))
      setError('')
      setNoDisk(false)
    } catch (err) {
      const message = errText(err, 'Loading the disk did not work.')
      // An app without storage is the common case, not a failure.
      if (/no disk|no storage|not have a disk/i.test(message)) {
        setNoDisk(true)
        setError('')
        return
      }
      setError(message)
    }
  }, [space, app, path])

  useEffect(() => {
    load()
  }, [load])

  async function run(label: string, fn: () => Promise<unknown>) {
    setBusy(label)
    setError('')
    try {
      await fn()
      await load()
    } catch (err) {
      setError(errText(err, 'That did not work.'))
    } finally {
      setBusy('')
    }
  }

  async function upload(files: FileList) {
    for (const file of Array.from(files)) {
      await run(`Uploading ${file.name}…`, () =>
        api.uploadFile(space, app, path ? `${path}/${file.name}` : file.name, file),
      )
    }
  }

  if (noDisk) return null

  const crumbs = path === '' ? [] : path.split('/')
  const empty = listing && listing.entries.length === 0 && (listing.folders?.length ?? 0) === 0

  // Inline editors claim Escape ("cancel this edit") so the drawer stays.
  const cancelOnEscape = (cancel: () => void) => (e: React.KeyboardEvent) => {
    if (e.key === 'Escape') {
      e.preventDefault()
      cancel()
    }
  }

  return (
    <section className="space-y-3">
      <SectionHeader
        title="Files"
        actions={
          <>
            <IconButton icon={Upload} label="Upload files (or drop them on the list)" onClick={() => fileInput.current?.click()} />
            <IconButton icon={FolderPlus} label="New folder" onClick={() => setNewFolder('')} />
            <IconButton icon={RefreshCw} label="Refresh files" onClick={load} />
          </>
        }
      />
      <input
        ref={fileInput}
        type="file"
        multiple
        className="hidden"
        onChange={(e) => {
          if (e.target.files?.length) upload(e.target.files)
          e.target.value = ''
        }}
      />
      <nav className="flex min-h-7 flex-wrap items-center gap-1 text-xs" aria-label="Disk path">
        <button
          type="button"
          onClick={() => setPath('')}
          className="flex h-7 items-center gap-1 rounded px-1.5 font-mono text-muted hover:bg-line/50 hover:text-ink"
        >
          <HardDrive className="size-3.5" aria-hidden /> disk
        </button>
        {crumbs.map((part, i) => (
          <span key={i} className="flex items-center gap-1">
            <ChevronRight className="size-3 text-muted" aria-hidden />
            <button
              type="button"
              onClick={() => setPath(crumbs.slice(0, i + 1).join('/'))}
              className="h-7 rounded px-1.5 font-mono hover:bg-line/50"
            >
              {part}
            </button>
          </span>
        ))}
        {busy && <span className="ml-auto text-muted">{busy}</span>}
      </nav>

      <ErrorNote message={error} />
      {listing === null && !error && <Spinner label="Opening the disk…" />}

      {/* One list; dropping files onto it uploads them into this folder. */}
      <div
        onDragOver={(e) => {
          e.preventDefault()
          setDragOver(true)
        }}
        onDragLeave={() => setDragOver(false)}
        onDrop={(e) => {
          e.preventDefault()
          setDragOver(false)
          if (e.dataTransfer.files.length) upload(e.dataTransfer.files)
        }}
        className={`rounded-lg transition-[outline-color] ${dragOver ? 'outline-2 outline-dashed outline-offset-2 outline-accent' : ''}`}
      >
        {listing && (
          <List>
            {newFolder !== null && (
              <ListRow className="bg-surface/40" actions={
                <>
                  <IconButton icon={Check} size="sm" label="Create folder" disabled={newFolder.trim() === ''} onClick={() => {
                    const name = newFolder.trim()
                    if (!name) return
                    setNewFolder(null)
                    run('Creating…', () => api.makeFolder(space, app, path ? `${path}/${name}` : name))
                  }} />
                  <IconButton icon={X} size="sm" label="Cancel" onClick={() => setNewFolder(null)} />
                </>
              }>
                <FolderPlus className="size-4 shrink-0 text-muted" aria-hidden />
                <Input bare autoFocus value={newFolder} placeholder="Folder name" aria-label="Name of the new folder"
                  onChange={(e) => setNewFolder(e.target.value)}
                  onKeyDown={(e) => {
                    cancelOnEscape(() => setNewFolder(null))(e)
                    if (e.key === 'Enter' && newFolder.trim()) {
                      const name = newFolder.trim()
                      setNewFolder(null)
                      run('Creating…', () => api.makeFolder(space, app, path ? `${path}/${name}` : name))
                    }
                  }}
                  className="flex-1" />
              </ListRow>
            )}
            {/* The top level shows donkey's own folder names, so say which
                app path each one is mounted at. */}
            {(listing.folders ?? []).map((f) => (
              <ListRow key={f.name}>
                <button type="button" onClick={() => setPath(f.name)} className="flex min-w-0 flex-1 items-center gap-3 text-left">
                  <Folder className="size-4 shrink-0 text-accent" aria-hidden />
                  <span className="font-mono text-sm">{f.name}</span>
                  <span className="truncate text-xs text-muted">mounted at {f.path}</span>
                </button>
              </ListRow>
            ))}
            {listing.entries.map((e) => {
              const full = path ? `${path}/${e.name}` : e.name
              if (renaming?.name === e.name) {
                const commit = () => {
                  const next = renaming.value.trim()
                  setRenaming(null)
                  if (next && next !== e.name) {
                    run('Renaming…', () => api.moveFile(space, app, full, path ? `${path}/${next}` : next))
                  }
                }
                return (
                  <ListRow key={e.name} className="bg-surface/40" actions={
                    <>
                      <IconButton icon={Check} size="sm" label="Rename" onClick={commit} />
                      <IconButton icon={X} size="sm" label="Cancel" onClick={() => setRenaming(null)} />
                    </>
                  }>
                    <Pencil className="size-4 shrink-0 text-muted" aria-hidden />
                    <Input bare autoFocus value={renaming.value} aria-label={`New name for ${e.name}`}
                      onChange={(ev) => setRenaming({ name: e.name, value: ev.target.value })}
                      onKeyDown={(ev) => {
                        cancelOnEscape(() => setRenaming(null))(ev)
                        if (ev.key === 'Enter') commit()
                      }}
                      className="flex-1" />
                  </ListRow>
                )
              }
              if (deleting === e.name) {
                return (
                  <ListRow key={e.name} className="bg-danger/10" actions={
                    <>
                      <Button kind="danger" size="sm" onClick={() => {
                        setDeleting('')
                        run('Removing…', () => api.removeFile(space, app, full))
                      }}>Delete</Button>
                      <IconButton icon={X} size="sm" label="Keep it" onClick={() => setDeleting('')} />
                    </>
                  }>
                    <Trash2 className="size-4 shrink-0 text-danger" aria-hidden />
                    <span className="min-w-0 flex-1 truncate text-sm text-danger">
                      Delete <span className="font-mono">{e.name}</span>{e.isDir ? ' and everything in it' : ''}? A redeploy does not bring it back.
                    </span>
                  </ListRow>
                )
              }
              return (
                <ListRow key={e.name} actions={
                  <>
                    {!e.isDir && (
                      <IconButton icon={Download} size="sm" label={`Download ${e.name}`} onClick={() =>
                        run(`Downloading ${e.name}…`, async () => saveBlob(await api.downloadFile(space, app, full), e.name))
                      } />
                    )}
                    <IconButton icon={Pencil} size="sm" label={`Rename ${e.name}`} onClick={() => setRenaming({ name: e.name, value: e.name })} />
                    <IconButton icon={Trash2} size="sm" kind="danger" label={`Delete ${e.name}`} onClick={() => setDeleting(e.name)} />
                  </>
                }>
                  {e.isDir ? (
                    <button type="button" onClick={() => setPath(full)} className="flex min-w-0 flex-1 items-center gap-3 text-left hover:underline">
                      <Folder className="size-4 shrink-0 text-accent" aria-hidden />
                      <span className="truncate font-mono text-sm">{e.name}</span>
                    </button>
                  ) : (
                    <span className="flex min-w-0 flex-1 items-center gap-3">
                      <File className="size-4 shrink-0 text-muted" aria-hidden />
                      <span className="truncate font-mono text-sm">{e.name}</span>
                    </span>
                  )}
                  <span className="shrink-0 text-xs text-muted">{e.isDir ? '' : humanSize(e.size)}</span>
                  <span className="hidden w-20 shrink-0 text-right text-xs text-muted sm:inline">
                    {new Date(e.modTime).toLocaleDateString()}
                  </span>
                </ListRow>
              )
            })}
            {empty && newFolder === null && (
              <ListRow><span className="text-sm text-muted">Nothing here yet — drop a file onto this list to upload it.</span></ListRow>
            )}
          </List>
        )}
      </div>

      {listing?.truncated && (
        <p className="text-xs text-muted">
          This folder holds more files than the console lists at once — open a subfolder to see the rest.
        </p>
      )}
    </section>
  )
}

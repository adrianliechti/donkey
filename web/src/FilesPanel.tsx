import { useCallback, useEffect, useRef, useState } from 'react'
import {
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
} from 'lucide-react'
import { api, type FileListing } from './api'
import { Button, ErrorNote, errText, Spinner } from './ui'

function humanSize(n: number): string {
  if (n < 1024) return `${n} B`
  const units = ['KB', 'MB', 'GB', 'TB']
  let size = n / 1024
  let i = 0
  while (size >= 1024 && i < units.length - 1) {
    size /= 1024
    i++
  }
  return `${size.toFixed(1)} ${units[i]}`
}

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

  return (
    <div className="space-y-3">
      <div className="flex flex-wrap items-center justify-between gap-2">
        {/* The breadcrumb is enough context inside the Files tab. */}
        <nav className="flex flex-wrap items-center gap-1 text-xs" aria-label="Disk path">
          <button
            type="button"
            onClick={() => setPath('')}
            className="flex items-center gap-1 rounded px-1 py-0.5 font-mono text-stone hover:bg-line/50 hover:text-ink"
          >
            <HardDrive className="size-3.5" aria-hidden /> disk
          </button>
          {crumbs.map((part, i) => (
            <span key={i} className="flex items-center gap-1">
              <ChevronRight className="size-3 text-stone" aria-hidden />
              <button
                type="button"
                onClick={() => setPath(crumbs.slice(0, i + 1).join('/'))}
                className="rounded px-1 py-0.5 font-mono hover:bg-line/50"
              >
                {part}
              </button>
            </span>
          ))}
        </nav>
        <div className="flex items-center gap-2">
          <button
            type="button"
            onClick={() => fileInput.current?.click()}
            className="flex items-center gap-1.5 rounded-md px-2 py-1 text-xs text-stone hover:bg-line/50 focus-visible:outline-2 focus-visible:outline-hay-deep"
          >
            <Upload className="size-3.5" aria-hidden /> Upload
          </button>
          <button
            type="button"
            onClick={() => setNewFolder('')}
            className="flex items-center gap-1.5 rounded-md px-2 py-1 text-xs text-stone hover:bg-line/50 focus-visible:outline-2 focus-visible:outline-hay-deep"
          >
            <FolderPlus className="size-3.5" aria-hidden /> New folder
          </button>
          <button
            type="button"
            onClick={load}
            title="Refresh files"
            aria-label="Refresh files"
            className="flex items-center rounded-md p-1.5 text-xs text-stone hover:bg-line/50 focus-visible:outline-2 focus-visible:outline-hay-deep"
          >
            <RefreshCw className="size-3.5" aria-hidden />
          </button>
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
        </div>
      </div>

      <ErrorNote message={error} />
      {busy && <p className="text-xs text-stone">{busy}</p>}

      {newFolder !== null && (
        <form
          className="flex items-center gap-2"
          onSubmit={(e) => {
            e.preventDefault()
            const name = newFolder.trim()
            if (!name) return
            setNewFolder(null)
            run('Creating…', () => api.makeFolder(space, app, path ? `${path}/${name}` : name))
          }}
        >
          <input
            value={newFolder}
            onChange={(e) => setNewFolder(e.target.value)}
            placeholder="folder name"
            autoFocus
            aria-label="Name of the new folder"
            className="flex-1 rounded-md border border-line bg-surface px-2 py-1 font-mono text-sm focus:outline-2 focus:outline-hay-deep"
          />
          <Button kind="quiet" type="submit" disabled={newFolder.trim() === ''}>
            Create
          </Button>
          <Button kind="quiet" onClick={() => setNewFolder(null)}>
            Cancel
          </Button>
        </form>
      )}

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
        className={`rounded-md border border-dashed p-1 transition-colors ${
          dragOver ? 'border-hay-deep bg-hay-wash' : 'border-transparent'
        }`}
      >
        {listing === null && !error && <Spinner label="Opening the disk…" />}

        {/* The top level shows donkey's own folder names, so say which app
            path each one is mounted at. */}
        {listing?.folders && listing.folders.length > 0 && (
          <ul className="mb-2 space-y-1">
            {listing.folders.map((f) => (
              <li key={f.name}>
                <button
                  onClick={() => setPath(f.name)}
                  className="flex w-full items-center gap-2 rounded px-2 py-1.5 text-left hover:bg-hay-wash/60"
                >
                  <Folder className="size-4 shrink-0 text-hay-deep" aria-hidden />
                  <span className="font-mono text-sm font-medium">{f.name}</span>
                  <span className="text-xs text-stone">→ {f.path} in the app</span>
                </button>
              </li>
            ))}
          </ul>
        )}

        {listing && listing.entries.length === 0 && (listing.folders?.length ?? 0) === 0 && (
          <p className="px-2 py-6 text-center text-sm text-stone">
            Nothing here yet — drop a file to upload it.
          </p>
        )}

        <ul className="divide-y divide-line">
          {listing?.entries.map((e) => {
            const full = path ? `${path}/${e.name}` : e.name
            if (renaming?.name === e.name) {
              return (
                <li key={e.name} className="px-2 py-1.5">
                  <form
                    className="flex items-center gap-2"
                    onSubmit={(ev) => {
                      ev.preventDefault()
                      const next = renaming.value.trim()
                      setRenaming(null)
                      if (next && next !== e.name) {
                        run('Renaming…', () =>
                          api.moveFile(space, app, full, path ? `${path}/${next}` : next),
                        )
                      }
                    }}
                  >
                    <input
                      value={renaming.value}
                      onChange={(ev) => setRenaming({ name: e.name, value: ev.target.value })}
                      autoFocus
                      aria-label={`New name for ${e.name}`}
                      className="flex-1 rounded-md border border-line bg-surface px-2 py-1 font-mono text-sm focus:outline-2 focus:outline-hay-deep"
                    />
                    <Button kind="quiet" type="submit">
                      Rename
                    </Button>
                    <Button kind="quiet" onClick={() => setRenaming(null)}>
                      Cancel
                    </Button>
                  </form>
                </li>
              )
            }
            if (deleting === e.name) {
              return (
                <li key={e.name} className="flex flex-wrap items-center gap-2 px-2 py-1.5">
                  <span className="flex-1 truncate font-mono text-sm text-brick">
                    Delete {e.name}
                    {e.isDir ? ' and everything in it' : ''}? Nothing on the disk comes back with a
                    redeploy.
                  </span>
                  <Button
                    kind="danger"
                    onClick={() => {
                      setDeleting('')
                      run('Removing…', () => api.removeFile(space, app, full))
                    }}
                  >
                    Delete
                  </Button>
                  <Button kind="quiet" onClick={() => setDeleting('')}>
                    Cancel
                  </Button>
                </li>
              )
            }
            return (
              <li key={e.name} className="flex items-center gap-2 px-2 py-1.5">
                {e.isDir ? (
                  <button
                    onClick={() => setPath(full)}
                    className="flex min-w-0 flex-1 items-center gap-2 text-left hover:underline"
                  >
                    <Folder className="size-4 shrink-0 text-hay-deep" aria-hidden />
                    <span className="truncate font-mono text-sm">{e.name}</span>
                  </button>
                ) : (
                  <span className="flex min-w-0 flex-1 items-center gap-2">
                    <File className="size-4 shrink-0 text-stone" aria-hidden />
                    <span className="truncate font-mono text-sm">{e.name}</span>
                  </span>
                )}
                <span className="shrink-0 text-xs text-stone">{e.isDir ? '—' : humanSize(e.size)}</span>
                <span className="hidden shrink-0 text-xs text-stone sm:inline">
                  {new Date(e.modTime).toLocaleDateString()}
                </span>
                <span className="flex shrink-0 items-center gap-0.5">
                  {!e.isDir && (
                    <button
                      aria-label={`Download ${e.name}`}
                      className="rounded p-1 text-stone hover:bg-line/50 hover:text-ink"
                      onClick={() =>
                        run(`Downloading ${e.name}…`, async () => {
                          // The download carries the token, so it goes through
                          // the API client and comes back as a blob.
                          const url = URL.createObjectURL(await api.downloadFile(space, app, full))
                          const a = document.createElement('a')
                          a.href = url
                          a.download = e.name
                          a.click()
                          URL.revokeObjectURL(url)
                        })
                      }
                    >
                      <Download className="size-4" aria-hidden />
                    </button>
                  )}
                  <button
                    onClick={() => setRenaming({ name: e.name, value: e.name })}
                    aria-label={`Rename ${e.name}`}
                    className="rounded p-1 text-stone hover:bg-line/50 hover:text-ink"
                  >
                    <Pencil className="size-4" aria-hidden />
                  </button>
                  <button
                    onClick={() => setDeleting(e.name)}
                    aria-label={`Delete ${e.name}`}
                    className="rounded p-1 text-stone hover:bg-brick/10 hover:text-brick"
                  >
                    <Trash2 className="size-4" aria-hidden />
                  </button>
                </span>
              </li>
            )
          })}
        </ul>

        {listing?.truncated && (
          <p className="px-2 py-2 text-xs text-stone">
            This folder holds more files than the console lists at once — open a subfolder to see the
            rest.
          </p>
        )}
      </div>
    </div>
  )
}

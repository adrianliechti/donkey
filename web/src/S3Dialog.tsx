import { useCallback, useEffect, useRef, useState } from 'react'
import {
  Archive,
  ChevronRight,
  Download,
  File,
  Folder,
  Plus,
  RefreshCw,
  Trash2,
  Upload,
} from 'lucide-react'
import { ApiError } from './api'
import { Modal, ModalBody, ModalHeader, ModalNotice, SplitPane } from './dialog'
import { Button, humanSize, IconButton, Input, saveBlob, Spinner, errText } from './ui'

interface Bucket {
  name: string
  createdAt: string
}

interface S3Object {
  key: string
  size: number
  modified: string
}

interface Listing {
  prefixes: string[]
  objects: S3Object[]
  truncated: boolean
}

// The dialog talks to the server directly: its S3 routes belong to it alone,
// so the shared client stays unaware of them.
const s3Path = (space: string, service: string, suffix: string) =>
  `/v1/spaces/${encodeURIComponent(space)}/services/${encodeURIComponent(service)}${suffix}`

async function s3Fetch(path: string, init?: RequestInit): Promise<Response> {
  const res = await fetch(path, {
    ...init,
    headers: init?.headers ?? {},
  })
  if (!res.ok) {
    let message = `The server answered HTTP ${res.status}`
    try {
      const body = await res.json()
      if (body.error) message = body.error
    } catch {
      /* not JSON */
    }
    throw new ApiError(res.status, message)
  }
  return res
}

async function s3Json<T>(path: string, init?: RequestInit): Promise<T> {
  return (await s3Fetch(path, init)).json() as Promise<T>
}

// The last path segment is what a person recognizes; the full key is what
// the store needs.
const lastSegment = (key: string) => key.replace(/\/$/, '').split('/').pop() ?? key

/**
 * Buckets and objects of a space's S3 service, in one dialog. The left rail
 * holds the buckets, the main pane walks the selected one folder by folder —
 * folders being nothing but key prefixes, the way S3 has always pretended.
 */
export function S3Dialog({
  space,
  service,
  onClose,
  onDelete,
}: {
  space: string
  service: string
  onClose: () => void
  onDelete: () => void
}) {
  const [buckets, setBuckets] = useState<Bucket[] | null>(null)
  const [bucket, setBucket] = useState('')
  const [prefix, setPrefix] = useState('')
  const [listing, setListing] = useState<Listing | null>(null)
  const [error, setError] = useState('')
  const [busy, setBusy] = useState('')
  const [newBucket, setNewBucket] = useState('')
  // Removing a bucket is the one destructive act with real reach, so it
  // confirms inline — and asks once more when the bucket turns out to hold
  // objects the console hasn't counted.
  const [removingBucket, setRemovingBucket] = useState<{ name: string; forced: boolean } | null>(null)
  const [deletingKey, setDeletingKey] = useState('')
  const fileInput = useRef<HTMLInputElement>(null)

  const loadBuckets = useCallback(async () => {
    try {
      const res = await s3Json<{ buckets: Bucket[] }>(s3Path(space, service, '/buckets'))
      const list = res.buckets ?? []
      setBuckets(list)
      // Opening straight into the first bucket saves the obvious click.
      setBucket((cur) => (cur && list.some((b) => b.name === cur) ? cur : (list[0]?.name ?? '')))
      setError('')
    } catch (err) {
      setBuckets([])
      setError(errText(err, 'Loading the buckets did not work.'))
    }
  }, [space, service])

  const loadObjects = useCallback(async () => {
    if (!bucket) {
      setListing(null)
      return
    }
    try {
      const res = await s3Json<Listing>(
        s3Path(
          space,
          service,
          `/buckets/${encodeURIComponent(bucket)}/objects?prefix=${encodeURIComponent(prefix)}`,
        ),
      )
      setListing({ prefixes: res.prefixes ?? [], objects: res.objects ?? [], truncated: res.truncated })
      setError('')
    } catch (err) {
      setListing(null)
      setError(errText(err, 'Loading the objects did not work.'))
    }
  }, [space, service, bucket, prefix])

  useEffect(() => {
    loadBuckets()
  }, [loadBuckets])

  useEffect(() => {
    setListing(null)
    loadObjects()
  }, [loadObjects])

  async function run(label: string, fn: () => Promise<unknown>) {
    setBusy(label)
    setError('')
    try {
      await fn()
    } catch (err) {
      setError(errText(err, 'That did not work.'))
    } finally {
      setBusy('')
    }
  }

  async function createBucket() {
    const name = newBucket.trim()
    if (!name) return
    await run(`Creating ${name}…`, async () => {
      await s3Json(s3Path(space, service, '/buckets'), {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ name }),
      })
      setNewBucket('')
      await loadBuckets()
      setBucket(name)
      setPrefix('')
    })
  }

  async function removeBucket(name: string, force: boolean) {
    setBusy(`Removing ${name}…`)
    setError('')
    try {
      await s3Json(
        s3Path(space, service, `/buckets/${encodeURIComponent(name)}${force ? '?force=true' : ''}`),
        { method: 'DELETE' },
      )
      setRemovingBucket(null)
      if (bucket === name) {
        setBucket('')
        setPrefix('')
      }
      await loadBuckets()
    } catch (err) {
      // A conflict means the bucket holds objects — that changes the
      // question, so ask it again rather than just failing.
      if (!force && err instanceof ApiError && err.status === 409) {
        setRemovingBucket({ name, forced: true })
      } else {
        setRemovingBucket(null)
        setError(errText(err, 'Removing did not work.'))
      }
    } finally {
      setBusy('')
    }
  }

  async function upload(files: FileList) {
    for (const file of Array.from(files)) {
      await run(`Uploading ${file.name}…`, async () => {
        const body = new FormData()
        body.append('file', file)
        await s3Json(
          s3Path(
            space,
            service,
            `/buckets/${encodeURIComponent(bucket)}/objects?key=${encodeURIComponent(prefix + file.name)}`,
          ),
          { method: 'POST', body },
        )
      })
    }
    await loadObjects()
  }

  async function download(key: string) {
    await run(`Downloading ${lastSegment(key)}…`, async () => {
      const res = await s3Fetch(
        s3Path(
          space,
          service,
          `/buckets/${encodeURIComponent(bucket)}/objects/download?key=${encodeURIComponent(key)}`,
        ),
      )
      saveBlob(await res.blob(), lastSegment(key))
    })
  }

  async function removeObject(key: string) {
    await run(`Removing ${lastSegment(key)}…`, async () => {
      await s3Json(
        s3Path(
          space,
          service,
          `/buckets/${encodeURIComponent(bucket)}/objects?key=${encodeURIComponent(key)}`,
        ),
        { method: 'DELETE' },
      )
      await loadObjects()
    })
  }

  const crumbs = prefix === '' ? [] : prefix.replace(/\/$/, '').split('/')

  return (
    <Modal onClose={() => !busy && onClose()} size="xl" height="fill">
      <ModalHeader eyebrow={`Buckets · ${space}`} title={<span className="font-mono">{service}</span>}
        actions={<>
          <IconButton icon={Upload} label="Upload files" disabled={!bucket || busy !== ''} onClick={() => fileInput.current?.click()} />
          <IconButton icon={RefreshCw} label="Reload the objects" disabled={!bucket} onClick={loadObjects} />
          <IconButton icon={Trash2} label="Delete service" kind="danger" onClick={onDelete} />
        </>}
        onClose={onClose} closeLabel="Close bucket browser" closeDisabled={busy !== ''} />
      <ModalNotice message={error} />
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
      <ModalBody padded={false}>
        <SplitPane
          sidebar={<>
            {/* The rail: every bucket, plus the way to a new one. */}
            <div className="min-h-0 flex-1 overflow-y-auto p-2">
              {buckets === null && <Spinner label="Opening the store…" />}
              {buckets !== null && buckets.length === 0 && (
                <p className="px-2 py-6 text-center text-sm text-muted">
                  No buckets yet — every object lives in one. Name your first below.
                </p>
              )}
              <ul className="space-y-0.5">
                {buckets?.map((b) =>
                  removingBucket?.name === b.name ? (
                    <li key={b.name} className="rounded-md border border-danger/30 bg-danger/5 p-2">
                      <p className="text-xs text-danger">
                        {removingBucket.forced
                          ? `${b.name} isn't empty — the console can't say how many objects it holds. Delete everything in it too?`
                          : `Delete the bucket ${b.name}?`}
                      </p>
                      <div className="mt-2 flex gap-1.5">
                        <Button
                          kind="danger"
                          size="sm"
                          disabled={busy !== ''}
                          onClick={() => removeBucket(b.name, removingBucket.forced)}
                        >
                          {removingBucket.forced ? 'Delete everything' : 'Delete'}
                        </Button>
                        <Button kind="quiet" size="sm" disabled={busy !== ''} onClick={() => setRemovingBucket(null)}>
                          Cancel
                        </Button>
                      </div>
                    </li>
                  ) : (
                    <li key={b.name} className="group flex items-center gap-1">
                      <button
                        onClick={() => {
                          setBucket(b.name)
                          setPrefix('')
                        }}
                        title={`Created ${new Date(b.createdAt).toLocaleDateString()}`}
                        className={`flex h-8 min-w-0 flex-1 items-center gap-2 rounded-md px-2 text-left font-mono text-xs ${
                          bucket === b.name ? 'bg-accent-soft text-ink' : 'text-ink-soft hover:bg-line/50'
                        }`}
                      >
                        <Archive className="size-3.5 shrink-0 text-accent-strong" aria-hidden />
                        <span className="truncate">{b.name}</span>
                      </button>
                      <IconButton
                        icon={Trash2}
                        size="sm"
                        kind="danger"
                        label={`Delete the bucket ${b.name}`}
                        onClick={() => setRemovingBucket({ name: b.name, forced: false })}
                        className=""
                      />
                    </li>
                  ),
                )}
              </ul>
            </div>
            <form
              className="flex items-center gap-1 border-t border-line p-2"
              onSubmit={(e) => {
                e.preventDefault()
                createBucket()
              }}
            >
              <Input
                size="sm"
                value={newBucket}
                onChange={(e) => setNewBucket(e.target.value)}
                placeholder="new bucket"
                aria-label="Name of the new bucket"
                className="flex-1"
              />
              <IconButton type="submit" icon={Plus} label="Create the bucket" disabled={newBucket.trim() === '' || busy !== ''} />
            </form>
          </>}
        >
          {/* The selected bucket, one folder level at a time. */}
          <div className="flex min-h-14 shrink-0 flex-wrap items-center justify-between gap-2 border-b border-line px-5">
            <nav className="flex min-w-0 flex-wrap items-center gap-1 text-xs" aria-label="Bucket path">
              <button
                onClick={() => setPrefix('')}
                disabled={!bucket}
                className="flex h-7 items-center gap-1 rounded px-1.5 font-mono text-muted hover:bg-line/50 hover:text-ink"
              >
                <Archive className="size-3.5" aria-hidden /> {bucket || '—'}
              </button>
              {crumbs.map((part, i) => (
                <span key={i} className="flex items-center gap-1">
                  <ChevronRight className="size-3 text-muted" aria-hidden />
                  <button
                    onClick={() => setPrefix(crumbs.slice(0, i + 1).join('/') + '/')}
                    className="h-7 rounded px-1.5 font-mono hover:bg-line/50"
                  >
                    {part}
                  </button>
                </span>
              ))}
            </nav>
            {busy && <span className="text-xs text-muted">{busy}</span>}
          </div>

          <div className="min-h-0 flex-1 overflow-y-auto px-3 py-2">
            {!bucket && buckets !== null && buckets.length > 0 && (
              <p className="px-2 py-6 text-center text-sm text-muted">
                Pick a bucket on the left to look inside.
              </p>
            )}
            {bucket && listing === null && !error && <Spinner label="Listing the objects…" />}
            {listing && listing.prefixes.length === 0 && listing.objects.length === 0 && (
              <p className="px-2 py-6 text-center text-sm text-muted">
                Nothing in here yet — upload a file to get things rolling.
              </p>
            )}

            <ul className="divide-y divide-line">
              {listing?.prefixes.map((p) => (
                <li key={p}>
                  <button
                    onClick={() => setPrefix(p)}
                    className="flex h-9 w-full items-center gap-2 rounded px-2 text-left hover:bg-accent-soft/60"
                  >
                    <Folder className="size-4 shrink-0 text-accent-strong" aria-hidden />
                    <span className="truncate font-mono text-sm">{lastSegment(p)}/</span>
                  </button>
                </li>
              ))}
              {listing?.objects.map((o) =>
                deletingKey === o.key ? (
                  <li key={o.key} className="flex flex-wrap items-center gap-2 px-2 py-1.5">
                    <span className="flex-1 truncate font-mono text-sm text-danger">
                      Delete {lastSegment(o.key)}? Objects don't come back.
                    </span>
                    <Button
                      kind="danger"
                      size="sm"
                      onClick={() => {
                        setDeletingKey('')
                        removeObject(o.key)
                      }}
                    >
                      Delete
                    </Button>
                    <Button kind="quiet" size="sm" onClick={() => setDeletingKey('')}>
                      Cancel
                    </Button>
                  </li>
                ) : (
                  <li key={o.key} className="flex h-9 items-center gap-2 px-2">
                    <span className="flex min-w-0 flex-1 items-center gap-2">
                      <File className="size-4 shrink-0 text-muted" aria-hidden />
                      <span className="truncate font-mono text-sm" title={o.key}>
                        {lastSegment(o.key)}
                      </span>
                    </span>
                    <span className="shrink-0 text-xs text-muted">{humanSize(o.size)}</span>
                    <span className="hidden shrink-0 text-xs text-muted sm:inline">
                      {new Date(o.modified).toLocaleDateString()}
                    </span>
                    <span className="flex shrink-0 items-center gap-0.5">
                      <IconButton icon={Download} size="sm" label={`Download ${lastSegment(o.key)}`} onClick={() => download(o.key)} />
                      <IconButton icon={Trash2} size="sm" kind="danger" label={`Delete ${lastSegment(o.key)}`} onClick={() => setDeletingKey(o.key)} />
                    </span>
                  </li>
                ),
              )}
            </ul>

            {listing?.truncated && (
              <p className="px-2 py-2 text-xs text-muted">
                This bucket holds more than the console lists at once — open a folder to see the
                rest.
              </p>
            )}
          </div>
        </SplitPane>
      </ModalBody>
    </Modal>
  )
}

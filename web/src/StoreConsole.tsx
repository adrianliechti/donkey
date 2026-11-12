import { useEffect, useRef, useState, type DragEvent, type ReactNode } from 'react'
import { useQuery } from '@tanstack/react-query'
import {
  Archive,
  ArrowDown,
  ArrowUp,
  Check,
  ChevronLeft,
  ChevronRight,
  Download,
  Folder,
  FolderPlus,
  HardDrive,
  Plus,
  RefreshCw,
  Trash2,
  Upload,
  X,
} from 'lucide-react'
import { ApiError } from './api'
import type { ConsoleProps } from './consoles'
import { Modal, ModalBody, ModalFooter, ModalHeader } from './dialog'
import { mutationError } from './queries'
import {
  downloadSelection,
  dropSources,
  kindOf,
  lastSegment,
  storeApi,
  useStoreContainers,
  useStoreObjects,
  useStoreWrites,
  useUploads,
  type UploadSource,
} from './storeApi'
import { listingNames, storeLabel, storeRows, type StoreSort } from './storeListing'
import { StorePreview } from './StorePreview'
import { BarSearch, Row } from './Workbench'
import { useServiceSearch, type ServiceSearch } from './serviceSearch'
import { Button, ErrorNote, Eyebrow, Field, IconButton, Input, Spinner, Toolbar } from './ui'
import { errText, humanSize } from './format'
import { serviceUI } from './serviceProtocol'

// The store browser: the containers (S3's buckets) on the left — unless the
// store is one tree, as a WebDAV service is — the open one folder by folder
// in the middle, the selected object on the right. Where it stands — bucket,
// folder, object, the filter — lives in the URL, so back and forward walk
// the folders a person opened and any of them can be linked.
type Place = { bucket?: string; prefix?: string; object?: string; q?: string }

type Uploads = ReturnType<typeof useUploads>
type Deletion = { bucket: string; keys: string[]; prefixes: string[] }

export function Console({ space, service, protocol }: ConsoleProps) {
  const [search, move] = useServiceSearch()
  const place = search as Place
  const go = (next: Place, how?: 'push' | 'replace') => move(next as ServiceSearch, how)
  // A WebDAV service is one tree: nothing to pick from on the left, the
  // files pane is the whole browser.
  const fixed = serviceUI(protocol).fixedRoot === true
  const buckets = useStoreContainers(space, service)
  // Opening straight into the first bucket saves the obvious click.
  const bucket = place.bucket ?? buckets.data?.[0]?.name ?? ''
  const prefix = place.prefix ?? ''
  const writes = useStoreWrites(space, service)
  const uploads = useUploads(space, service, writes.refreshObjects)
  const [creatingBucket, setCreatingBucket] = useState(false)
  const [deleting, setDeleting] = useState<Deletion | null>(null)
  // The order holds while walking from folder to folder.
  const [sort, setSort] = useState<StoreSort>({ by: 'name', desc: false })

  // A narrow screen shows one pane at a time: the buckets until one is
  // picked, then its folder, then the object.
  const picked = fixed || place.bucket !== undefined || place.object !== undefined
  const previewing = place.object !== undefined && bucket !== ''

  return (
    <Row>
      {!fixed && (
        <BucketList
          space={space}
          service={service}
          current={bucket}
          creating={creatingBucket}
          onCreating={setCreatingBucket}
          onOpen={(name) => go({ bucket: name })}
          onDeleted={(name) => {
            if (name === bucket) go({}, 'replace')
          }}
          className={picked ? 'hidden md:flex' : 'flex'}
        />
      )}
      <FilesPane
        key={`${bucket}\n${prefix}`}
        space={space}
        service={service}
        bucket={bucket}
        prefix={prefix}
        filter={place.q ?? ''}
        current={place.object}
        bucketsReady={buckets.data !== undefined}
        noBuckets={buckets.data?.length === 0}
        bucketsError={buckets.error ? errText(buckets.error, 'The store did not answer.') : ''}
        fixed={fixed}
        uploads={uploads}
        sort={sort}
        onSort={setSort}
        onFolder={(p) => go({ bucket, prefix: p || undefined })}
        onObject={(key) => go({ bucket, prefix: place.prefix, q: place.q, object: key })}
        onFilter={(q) => go({ bucket, prefix: place.prefix, object: place.object, q: q || undefined }, 'replace')}
        onBuckets={() => go({})}
        onNewBucket={() => setCreatingBucket(true)}
        className={previewing ? 'hidden xl:flex' : picked ? 'flex' : 'hidden md:flex'}
      />
      {previewing && (
        <StorePreview
          key={`${bucket}\n${place.object}`}
          space={space}
          service={service}
          bucket={bucket}
          objectKey={place.object!}
          protocol={protocol}
          onClose={() => go({ bucket, prefix: place.prefix, q: place.q })}
          onDelete={() => setDeleting({ bucket, keys: [place.object!], prefixes: [] })}
          className="min-w-0 flex-1 xl:w-96 xl:flex-none"
        />
      )}
      {deleting && (
        <DeleteObjects
          space={space}
          service={service}
          deletion={deleting}
          onClose={() => setDeleting(null)}
          onDeleted={() => {
            setDeleting(null)
            go({ bucket, prefix: place.prefix, q: place.q }, 'replace')
          }}
        />
      )}
    </Row>
  )
}

// ─── Buckets ───────────────────────────────────────────────────────────

// S3's own rules, checked before the store answers with an XML error.
const bucketName = /^(?!.*\.\.)[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$/

function BucketList({
  space,
  service,
  current,
  creating,
  onCreating,
  onOpen,
  onDeleted,
  className,
}: {
  space: string
  service: string
  current: string
  creating: boolean
  onCreating: (on: boolean) => void
  onOpen: (name: string) => void
  onDeleted: (name: string) => void
  className: string
}) {
  const { data: buckets, error, isFetching, refetch } = useStoreContainers(space, service)
  const { createBucket } = useStoreWrites(space, service)
  const [name, setName] = useState('')
  const [removing, setRemoving] = useState('')
  const valid = bucketName.test(name)
  const close = () => {
    onCreating(false)
    setName('')
    createBucket.reset()
  }

  return (
    <aside className={`w-full shrink-0 flex-col overflow-hidden bg-surface/60 md:w-64 ${className}`}>
      <div className="flex h-11 shrink-0 items-center justify-between gap-2 border-b border-line pr-1.5 pl-3">
        <Eyebrow>Buckets</Eyebrow>
        <Toolbar>
          <IconButton
            icon={RefreshCw}
            label="Refresh the buckets"
            onClick={() => void refetch()}
            className={isFetching ? '[&>svg]:animate-spin' : ''}
          />
          <IconButton icon={Plus} label="New bucket" onClick={() => onCreating(true)} />
        </Toolbar>
      </div>
      {creating && (
        <form
          className="shrink-0 space-y-1.5 border-b border-line bg-raised/40 p-2"
          onSubmit={(e) => {
            e.preventDefault()
            if (!valid) return
            createBucket.mutate(name, {
              onSuccess: () => {
                onOpen(name)
                close()
              },
            })
          }}
        >
          <div className="flex items-center gap-1">
            <Input
              size="sm"
              autoFocus
              value={name}
              onChange={(e) => setName(e.target.value.toLowerCase())}
              onKeyDown={(e) => {
                if (e.key === 'Escape') {
                  e.preventDefault()
                  close()
                }
              }}
              placeholder="new-bucket"
              aria-label="Name of the new bucket"
              className="min-w-0 flex-1"
            />
            <IconButton
              icon={Check}
              size="sm"
              type="submit"
              label="Create the bucket"
              disabled={!valid || createBucket.isPending}
            />
            <IconButton icon={X} size="sm" label="Cancel" onClick={close} />
          </div>
          {name !== '' && !valid && (
            <p className="px-0.5 text-[11px] text-muted">Lowercase letters, digits, dots and dashes; 3 to 63.</p>
          )}
          <ErrorNote message={mutationError(createBucket, 'Creating the bucket did not work.')} />
        </form>
      )}
      <nav aria-label="Buckets" className="min-h-0 flex-1 overflow-y-auto py-1">
        {error ? (
          <div className="p-2">
            <ErrorNote message={errText(error, 'The store did not answer.')} />
          </div>
        ) : !buckets ? (
          <Spinner label="Opening the store…" />
        ) : buckets.length === 0 ? (
          <p className="px-3 py-6 text-center text-xs leading-relaxed text-muted">No buckets yet.</p>
        ) : (
          buckets.map((b) => (
            <div key={b.name} className="group flex items-center pr-1">
              <button
                type="button"
                onClick={() => onOpen(b.name)}
                aria-current={b.name === current || undefined}
                title={`Created ${new Date(b.createdAt).toLocaleString()}`}
                className={`flex h-8 min-w-0 flex-1 items-center gap-2 rounded-md px-2 text-left font-mono text-xs transition-colors focus-visible:outline-2 focus-visible:-outline-offset-2 focus-visible:outline-accent-strong ${
                  b.name === current ? 'bg-accent-soft text-ink' : 'text-ink-soft hover:bg-line/50 hover:text-ink'
                }`}
              >
                <Archive className="size-3.5 shrink-0 text-muted" aria-hidden />
                <span className="truncate">{b.name}</span>
              </button>
              <IconButton
                icon={Trash2}
                size="sm"
                kind="danger"
                label={`Delete the bucket ${b.name}…`}
                onClick={() => setRemoving(b.name)}
                // Out of the way until the row is pointed at, or open.
                className={b.name === current ? '' : 'opacity-0 group-hover:opacity-100 focus-visible:opacity-100'}
              />
            </div>
          ))
        )}
      </nav>
      {buckets && buckets.length > 0 && (
        <footer className="flex h-9 shrink-0 items-center border-t border-line px-3 text-xs text-muted">
          {buckets.length} {buckets.length === 1 ? 'bucket' : 'buckets'}
        </footer>
      )}
      {removing && (
        <DeleteBucket
          space={space}
          service={service}
          name={removing}
          onClose={() => setRemoving('')}
          onDeleted={() => {
            onDeleted(removing)
            setRemoving('')
          }}
        />
      )}
    </aside>
  )
}

// Deleting a bucket is the act with the most reach, so a bucket that still
// holds objects asks for its name to be typed before it goes with them.
function DeleteBucket({
  space,
  service,
  name,
  onClose,
  onDeleted,
}: {
  space: string
  service: string
  name: string
  onClose: () => void
  onDeleted: () => void
}) {
  const { deleteBucket } = useStoreWrites(space, service)
  const peek = useQuery({
    queryKey: ['s3', space, service, 'peek', name],
    queryFn: () => storeApi.list(space, service, name, '', '', { recursive: true, max: 1 }),
    staleTime: 0,
    gcTime: 0,
  })
  const [conflict, setConflict] = useState(false)
  const [typed, setTyped] = useState('')
  const full = conflict || (peek.data !== undefined && peek.data.objects.length + peek.data.prefixes.length > 0)
  const busy = deleteBucket.isPending
  const run = () =>
    deleteBucket.mutate(
      { name, force: full },
      {
        onSuccess: onDeleted,
        // Something landed in it since the look: ask the bigger question.
        onError: (err) => {
          if (!full && err instanceof ApiError && err.status === 409) {
            setConflict(true)
            deleteBucket.reset()
          }
        },
      },
    )
  return (
    <Modal onClose={() => !busy && onClose()} size="sm">
      <ModalHeader
        eyebrow="Bucket"
        title={
          <>
            Delete <span className="font-mono">{name}</span>?
          </>
        }
        onClose={onClose}
        closeDisabled={busy}
      />
      <ModalBody>
        {peek.isPending ? (
          <Spinner label="Looking inside…" inline />
        ) : full ? (
          <div className="space-y-3">
            <p className="text-sm leading-relaxed text-ink-soft">
              <span className="font-mono text-ink">{name}</span> still holds objects. Deleting it deletes every one of
              them — the console cannot say how many — and none come back.
            </p>
            <Field label="Type the bucket's name to go ahead">
              <Input
                value={typed}
                onChange={(e) => setTyped(e.target.value)}
                autoFocus
                aria-label="Bucket name"
                placeholder={name}
              />
            </Field>
          </div>
        ) : (
          <p className="text-sm leading-relaxed text-ink-soft">The bucket is empty; only its name goes.</p>
        )}
        {(deleteBucket.error || peek.error) && (
          <div className="mt-3">
            <ErrorNote
              message={
                deleteBucket.error
                  ? mutationError(deleteBucket, 'Deleting did not work.')
                  : errText(peek.error, 'Looking inside did not work.')
              }
            />
          </div>
        )}
      </ModalBody>
      <ModalFooter>
        <Button kind="quiet" onClick={onClose} disabled={busy}>
          Keep it
        </Button>
        <Button kind="danger" icon={Trash2} onClick={run} disabled={busy || peek.isPending || (full && typed !== name)}>
          {busy ? 'Deleting…' : full ? 'Delete everything' : 'Delete bucket'}
        </Button>
      </ModalFooter>
    </Modal>
  )
}

// ─── One folder ────────────────────────────────────────────────────────

function FilesPane({
  space,
  service,
  bucket,
  prefix,
  filter,
  current,
  bucketsReady,
  noBuckets,
  bucketsError,
  fixed,
  uploads,
  sort,
  onSort,
  onFolder,
  onObject,
  onFilter,
  onBuckets,
  onNewBucket,
  className,
}: {
  space: string
  service: string
  bucket: string
  prefix: string
  filter: string
  current?: string
  bucketsReady: boolean
  noBuckets: boolean
  bucketsError: string
  fixed: boolean
  uploads: Uploads
  sort: StoreSort
  onSort: (sort: StoreSort) => void
  onFolder: (prefix: string) => void
  onObject: (key: string) => void
  onFilter: (q: string) => void
  onBuckets: () => void
  onNewBucket: () => void
  className: string
}) {
  const listing = useStoreObjects(space, service, bucket, prefix)
  const writes = useStoreWrites(space, service)
  const [selected, setSelected] = useState<Set<string>>(() => new Set())
  const anchor = useRef<number | null>(null)
  const [newFolder, setNewFolder] = useState<string | null>(null)
  const [deleting, setDeleting] = useState<Deletion | null>(null)
  const [replacing, setReplacing] = useState<{ sources: UploadSource[]; conflicts: string[] } | null>(null)
  const [downloading, setDownloading] = useState('')
  const [downloadError, setDownloadError] = useState('')
  const fileInput = useRef<HTMLInputElement>(null)
  const drag = useRef(0)
  const [dragging, setDragging] = useState(false)

  // The filter box answers at once; the URL catches up when typing pauses.
  const [text, setText] = useState(filter)
  const [synced, setSynced] = useState(filter)
  if (synced !== filter) {
    setSynced(filter)
    setText(filter)
  }
  const latest = useRef(onFilter)
  useEffect(() => {
    latest.current = onFilter
  })
  useEffect(() => {
    if (text === filter) return
    const timer = setTimeout(() => latest.current(text), 250)
    return () => clearTimeout(timer)
  }, [text, filter])

  const pages = listing.data?.pages ?? []
  const objects = [...new Map(pages.flatMap((p) => p.objects).map((o) => [o.key, o])).values()]
  const names = listingNames(pages)
  const needle = text.trim().toLowerCase()
  const rows = storeRows(pages, text, sort)
  const picked = rows.filter((r) => selected.has(r.id))
  const allPicked = rows.length > 0 && picked.length === rows.length

  const toggle = (index: number, range: boolean) => {
    // Shift extends from the last row clicked, the way file lists do.
    const from = range && anchor.current !== null ? Math.min(anchor.current, index) : index
    const to = range && anchor.current !== null ? Math.max(anchor.current, index) : index
    const ids = rows.slice(from, to + 1).map((r) => r.id)
    const on = !selected.has(rows[index].id)
    anchor.current = index
    setSelected((cur) => {
      const next = new Set(cur)
      for (const id of ids) {
        if (on) next.add(id)
        else next.delete(id)
      }
      return next
    })
  }

  const upload = (sources: UploadSource[]) => {
    if (sources.length === 0 || !bucket) return
    const existing = new Set(objects.map((o) => o.key))
    const conflicts = sources.filter((s) => existing.has(prefix + s.path)).map((s) => s.path)
    if (conflicts.length > 0) setReplacing({ sources, conflicts })
    else uploads.add(bucket, prefix, sources)
  }

  const onDrop = async (e: DragEvent) => {
    e.preventDefault()
    drag.current = 0
    setDragging(false)
    if (!bucket) return
    upload(await dropSources(e.dataTransfer))
  }
  const hasFiles = (e: DragEvent) => Array.from(e.dataTransfer.types).includes('Files')

  const download = async () => {
    setDownloadError('')
    try {
      await downloadSelection(
        space,
        service,
        bucket,
        prefix,
        picked.filter((r) => !r.folder).map((r) => r.key),
        picked.filter((r) => r.folder).map((r) => r.key),
        setDownloading,
      )
    } catch (err) {
      setDownloadError(errText(err, 'The download did not work.'))
    } finally {
      setDownloading('')
    }
  }

  const crumbs = prefix === '' ? [] : prefix.replace(/\/$/, '').split('/')
  const loading = bucket !== '' && listing.isPending
  const empty = !loading && !listing.error && rows.length === 0 && newFolder === null

  if (!bucket) {
    return (
      <section className={`min-w-0 flex-1 flex-col items-center justify-center gap-3 p-8 text-center ${className}`}>
        {bucketsError ? (
          <ErrorNote message={bucketsError} />
        ) : !bucketsReady ? (
          <Spinner label="Opening the store…" />
        ) : noBuckets ? (
          <>
            <Archive className="size-6 text-muted" aria-hidden />
            <p className="max-w-sm text-sm text-muted">No buckets yet — every object lives in one.</p>
            <Button kind="quiet" size="sm" icon={Plus} onClick={onNewBucket}>
              New bucket
            </Button>
          </>
        ) : null}
      </section>
    )
  }

  return (
    <section
      aria-label={`${bucket}/${prefix}`}
      className={`relative min-w-0 flex-1 flex-col overflow-hidden ${className}`}
      onDragEnter={(e) => {
        if (!hasFiles(e)) return
        e.preventDefault()
        drag.current++
        setDragging(true)
      }}
      onDragOver={(e) => {
        if (hasFiles(e)) e.preventDefault()
      }}
      onDragLeave={() => {
        drag.current = Math.max(0, drag.current - 1)
        if (drag.current === 0) setDragging(false)
      }}
      onDrop={(e) => void onDrop(e)}
    >
      <input
        ref={fileInput}
        type="file"
        multiple
        className="hidden"
        onChange={(e) => {
          upload(Array.from(e.target.files ?? []).map((file) => ({ file, path: file.name })))
          e.target.value = ''
        }}
      />
      <div className="flex min-h-11 shrink-0 flex-wrap items-center justify-between gap-x-3 gap-y-1 border-b border-line py-1 pr-1.5 pl-2">
        <nav aria-label="Folder" className="flex min-w-0 flex-wrap items-center gap-0.5 text-sm">
          {!fixed && (
            <IconButton icon={ChevronLeft} label="Back to the buckets" onClick={onBuckets} className="md:hidden" />
          )}
          <FolderCrumb
            space={space}
            service={service}
            bucket={bucket}
            folder=""
            displayName={names.get('')}
            fallbackLabel={bucket}
            fixed={fixed}
            onClick={crumbs.length > 0 ? () => onFolder('') : undefined}
          >
            {fixed ? (
              <HardDrive className="size-3.5 shrink-0" aria-hidden />
            ) : (
              <Archive className="size-3.5 shrink-0" aria-hidden />
            )}
          </FolderCrumb>
          {crumbs.map((_, i) => (
            <span key={i} className="flex min-w-0 items-center gap-0.5">
              <ChevronRight className="size-3.5 shrink-0 text-muted" aria-hidden />
              <FolderCrumb
                space={space}
                service={service}
                bucket={bucket}
                folder={crumbs.slice(0, i + 1).join('/') + '/'}
                displayName={names.get(crumbs.slice(0, i + 1).join('/') + '/')}
                fixed={fixed}
                onClick={i < crumbs.length - 1 ? () => onFolder(crumbs.slice(0, i + 1).join('/') + '/') : undefined}
              />
            </span>
          ))}
        </nav>
        <Toolbar>
          <BarSearch value={text} onChange={setText} placeholder="Filter" label="Filter this folder" className="w-32" />
          <IconButton icon={FolderPlus} label="New folder" onClick={() => setNewFolder('')} />
          <IconButton
            icon={Upload}
            label="Upload files (or drop them here)"
            onClick={() => fileInput.current?.click()}
          />
          <IconButton
            icon={RefreshCw}
            label="Refresh"
            onClick={() => void listing.refetch()}
            className={listing.isFetching && !listing.isFetchingNextPage ? '[&>svg]:animate-spin' : ''}
          />
        </Toolbar>
      </div>

      <div className="min-h-0 flex-1 overflow-auto">
        <table className="w-full table-fixed border-collapse text-left">
          <thead className="sticky top-0 z-10 bg-card">
            <tr className="h-9 border-b border-line">
              <th className="w-10 pl-3">
                <input
                  type="checkbox"
                  aria-label="Select everything listed"
                  checked={allPicked}
                  disabled={rows.length === 0}
                  ref={(el) => {
                    if (el) el.indeterminate = picked.length > 0 && !allPicked
                  }}
                  onChange={() => setSelected(allPicked ? new Set() : new Set(rows.map((r) => r.id)))}
                />
              </th>
              <SortHead sort={sort} by="name" onSort={onSort}>
                Name
              </SortHead>
              <SortHead sort={sort} by="size" onSort={onSort} className="w-24 text-right">
                Size
              </SortHead>
              <SortHead sort={sort} by="modified" onSort={onSort} className="hidden w-44 lg:table-cell">
                Modified
              </SortHead>
              <th className="hidden w-24 pr-3 font-mono text-[10px] font-semibold tracking-[0.14em] text-muted uppercase sm:table-cell">
                Type
              </th>
            </tr>
          </thead>
          <tbody>
            {newFolder !== null && (
              <NewFolderRow
                busy={writes.createFolder.isPending}
                error={mutationError(writes.createFolder, 'Creating the folder did not work.')}
                value={newFolder}
                onChange={setNewFolder}
                onCancel={() => {
                  setNewFolder(null)
                  writes.createFolder.reset()
                }}
                onCreate={(name) =>
                  writes.createFolder.mutate(
                    { bucket, key: `${prefix}${name}/` },
                    { onSuccess: () => setNewFolder(null) },
                  )
                }
              />
            )}
            {rows.map((row, i) => {
              const on = selected.has(row.id)
              const open = !row.folder && row.key === current
              const kind = row.folder ? null : kindOf(row.key)
              const Icon = kind?.icon ?? Folder
              return (
                <tr
                  key={row.id}
                  onClick={() => (row.folder ? onFolder(row.key) : onObject(row.key))}
                  className={`h-9 cursor-pointer border-b border-line/60 transition-colors ${
                    open ? 'bg-accent-soft' : on ? 'bg-accent-soft/40' : 'hover:bg-raised/60'
                  }`}
                >
                  <td className="pl-3" onClick={(e) => e.stopPropagation()}>
                    <input
                      type="checkbox"
                      checked={on}
                      aria-label={`Select ${row.label}`}
                      onChange={(e) => toggle(i, (e.nativeEvent as MouseEvent).shiftKey)}
                    />
                  </td>
                  <td className="min-w-0 pr-3">
                    <button
                      type="button"
                      onClick={(e) => {
                        e.stopPropagation()
                        if (row.folder) onFolder(row.key)
                        else onObject(row.key)
                      }}
                      title={row.key}
                      className="flex max-w-full min-w-0 items-center gap-2 rounded text-left focus-visible:outline-2 focus-visible:outline-accent-strong"
                    >
                      <Icon className={`size-4 shrink-0 ${row.folder ? 'text-accent' : 'text-muted'}`} aria-hidden />
                      <span className="truncate font-mono text-[13px] text-ink">
                        {row.label}
                        {row.folder && <span className="text-muted">/</span>}
                      </span>
                    </button>
                  </td>
                  <td className="pr-3 text-right font-mono text-xs whitespace-nowrap text-muted">
                    {row.object ? humanSize(row.object.size) : ''}
                  </td>
                  <td
                    className="hidden pr-3 text-xs whitespace-nowrap text-muted lg:table-cell"
                    title={row.object?.modified}
                  >
                    {row.object?.modified
                      ? new Date(row.object.modified).toLocaleString(undefined, {
                          dateStyle: 'medium',
                          timeStyle: 'short',
                        })
                      : ''}
                  </td>
                  <td className="hidden truncate pr-3 text-xs text-muted sm:table-cell">
                    {row.folder ? 'Folder' : kind?.label}
                  </td>
                </tr>
              )
            })}
          </tbody>
        </table>
        {listing.error ? (
          <div className="space-y-3 p-4">
            <ErrorNote message={errText(listing.error, 'Listing the folder did not work.')} />
            <Button kind="quiet" size="sm" icon={RefreshCw} onClick={() => void listing.refetch()}>
              Try again
            </Button>
          </div>
        ) : loading ? (
          <Spinner label="Listing the folder…" />
        ) : empty ? (
          <p className="px-4 py-10 text-center text-sm text-muted">
            {needle
              ? `Nothing here is named like “${text.trim()}”${listing.hasNextPage ? ' among what is loaded' : ''}.`
              : prefix
                ? 'This folder is empty — drop files here to upload them.'
                : fixed
                  ? 'Nothing here yet — drop files here, or make a folder.'
                  : 'This bucket is empty — drop files here, or make a folder.'}
          </p>
        ) : null}
        {listing.hasNextPage && (
          <div className="flex items-center justify-center gap-3 p-3">
            {needle && <span className="text-xs text-muted">The filter covers what is loaded.</span>}
            <Button
              kind="quiet"
              size="sm"
              disabled={listing.isFetchingNextPage}
              onClick={() => void listing.fetchNextPage()}
            >
              {listing.isFetchingNextPage ? 'Loading…' : 'Load more'}
            </Button>
          </div>
        )}
      </div>

      {downloadError && (
        <div className="shrink-0 border-t border-line p-3">
          <ErrorNote message={downloadError} />
        </div>
      )}
      {picked.length > 0 && (
        <div className="flex min-h-11 shrink-0 flex-wrap items-center justify-between gap-2 border-t border-line bg-raised/40 py-1.5 pr-1.5 pl-3 text-xs">
          <span className="text-ink">
            {picked.length} selected
            {downloading && <span className="text-muted"> · {downloading}</span>}
          </span>
          <Toolbar>
            <Button
              kind="quiet"
              size="sm"
              icon={Download}
              disabled={downloading !== ''}
              onClick={() => void download()}
            >
              {picked.length === 1 && !picked[0].folder ? 'Download' : 'Download .zip'}
            </Button>
            <Button
              kind="danger"
              size="sm"
              icon={Trash2}
              onClick={() =>
                setDeleting({
                  bucket,
                  keys: picked.filter((r) => !r.folder).map((r) => r.key),
                  prefixes: picked.filter((r) => r.folder).map((r) => r.key),
                })
              }
            >
              Delete…
            </Button>
            <IconButton icon={X} size="sm" label="Clear the selection" onClick={() => setSelected(new Set())} />
          </Toolbar>
        </div>
      )}
      <UploadTray uploads={uploads} />

      {dragging && (
        <div className="pointer-events-none absolute inset-2 z-20 flex items-center justify-center rounded-lg border-2 border-dashed border-accent bg-accent-soft/70">
          <p className="flex items-center gap-2 text-sm font-medium text-ink">
            <Upload className="size-4" aria-hidden />
            Drop to upload into{' '}
            <span className="font-mono">
              {bucket}/{prefix}
            </span>
          </p>
        </div>
      )}
      {deleting && (
        <DeleteObjects
          space={space}
          service={service}
          deletion={deleting}
          onClose={() => setDeleting(null)}
          onDeleted={() => {
            setDeleting(null)
            setSelected(new Set())
            // The open object went with the selection: close it too.
            if (current && (deleting.keys.includes(current) || deleting.prefixes.some((p) => current.startsWith(p))))
              onFolder(prefix)
          }}
        />
      )}
      {replacing && (
        <ReplaceUploads
          conflicts={replacing.conflicts}
          onClose={() => setReplacing(null)}
          onReplace={() => {
            uploads.add(bucket, prefix, replacing.sources)
            setReplacing(null)
          }}
          onSkip={() => {
            uploads.add(
              bucket,
              prefix,
              replacing.sources.filter((s) => !replacing.conflicts.includes(s.path)),
            )
            setReplacing(null)
          }}
          skippable={replacing.sources.length > replacing.conflicts.length}
        />
      )}
    </section>
  )
}

function FolderCrumb({
  space,
  service,
  bucket,
  folder,
  displayName,
  fallbackLabel = '',
  fixed,
  onClick,
  children,
}: {
  space: string
  service: string
  bucket: string
  folder: string
  displayName?: string
  fallbackLabel?: string
  fixed: boolean
  onClick?: () => void
  children?: ReactNode
}) {
  // The listing describes the folder itself, too. Reuse its cache while
  // walking, and resolve ancestors on a direct link into a WebDAV tree.
  const listing = useStoreObjects(space, service, bucket, folder, fixed && !displayName)
  const label = displayName || storeLabel(folder, listingNames(listing.data?.pages ?? [])) || fallbackLabel
  return (
    <Crumb onClick={onClick}>
      {children}
      {label}
    </Crumb>
  )
}

function Crumb({ onClick, children }: { onClick?: () => void; children: ReactNode }) {
  const cls = 'flex max-w-64 min-w-0 items-center gap-1.5 truncate rounded px-1.5 py-0.5 font-mono text-[13px]'
  if (!onClick) {
    return (
      <span aria-current="page" className={`${cls} font-medium text-ink`}>
        {children}
      </span>
    )
  }
  return (
    <button
      type="button"
      onClick={onClick}
      className={`${cls} text-muted underline-offset-2 hover:text-ink hover:underline focus-visible:outline-2 focus-visible:outline-accent-strong`}
    >
      {children}
    </button>
  )
}

function SortHead({
  sort,
  by,
  onSort,
  className = '',
  children,
}: {
  sort: StoreSort
  by: StoreSort['by']
  onSort: (sort: StoreSort) => void
  className?: string
  children: string
}) {
  const active = sort.by === by
  const Arrow = sort.desc ? ArrowDown : ArrowUp
  return (
    <th aria-sort={active ? (sort.desc ? 'descending' : 'ascending') : undefined} className={`pr-3 ${className}`}>
      <button
        type="button"
        onClick={() => onSort({ by, desc: active ? !sort.desc : by === 'modified' })}
        className={`inline-flex items-center gap-1 rounded font-mono text-[10px] font-semibold tracking-[0.14em] uppercase hover:text-ink focus-visible:outline-2 focus-visible:outline-accent-strong ${
          active ? 'text-ink' : 'text-muted'
        }`}
      >
        {children}
        {active && <Arrow className="size-3" aria-hidden />}
      </button>
    </th>
  )
}

function NewFolderRow({
  value,
  onChange,
  onCreate,
  onCancel,
  busy,
  error,
}: {
  value: string
  onChange: (v: string) => void
  onCreate: (name: string) => void
  onCancel: () => void
  busy: boolean
  error: string
}) {
  // "a/b" makes both; no empty or dot segments, which S3 allows and
  // nothing else understands.
  const name = value.trim().replace(/^\/+|\/+$/g, '')
  const valid = name !== '' && name.split('/').every((part) => part !== '' && part !== '.' && part !== '..')
  const commit = () => valid && !busy && onCreate(name)
  return (
    <>
      <tr className="h-10 border-b border-line/60 bg-raised/40">
        <td className="pl-3">
          <FolderPlus className="size-4 text-muted" aria-hidden />
        </td>
        <td colSpan={4} className="pr-1.5">
          <div className="flex items-center gap-1">
            <Input
              size="sm"
              autoFocus
              value={value}
              onChange={(e) => onChange(e.target.value)}
              onKeyDown={(e) => {
                if (e.key === 'Enter') {
                  e.preventDefault()
                  commit()
                }
                if (e.key === 'Escape') {
                  e.preventDefault()
                  onCancel()
                }
              }}
              placeholder="Folder name"
              aria-label="Name of the new folder"
              className="min-w-0 flex-1"
            />
            <IconButton icon={Check} size="sm" label="Create the folder" disabled={!valid || busy} onClick={commit} />
            <IconButton icon={X} size="sm" label="Cancel" onClick={onCancel} />
          </div>
        </td>
      </tr>
      {error && (
        <tr>
          <td colSpan={5} className="px-3 py-2">
            <ErrorNote message={error} />
          </td>
        </tr>
      )}
    </>
  )
}

// ─── Uploads, deletes ──────────────────────────────────────────────────

function UploadTray({ uploads }: { uploads: Uploads }) {
  const { items, cancel, clear, active } = uploads
  const failed = items.filter((it) => it.state === 'failed').length
  const [open, setOpen] = useState(false)
  if (items.length === 0) return null
  const done = items.filter((it) => it.state === 'done').length
  const cancelled = items.filter((it) => it.state === 'cancelled').length
  const total = items.reduce((n, it) => n + it.size, 0)
  const loaded = items.reduce((n, it) => n + (it.state === 'done' ? it.size : it.loaded), 0)
  const pct = total === 0 ? (active ? 0 : 100) : Math.round((loaded / total) * 100)
  return (
    <div className="shrink-0 border-t border-line">
      <div className="flex min-h-10 items-center gap-3 py-1 pr-1.5 pl-3 text-xs">
        <button
          type="button"
          onClick={() => setOpen(!open)}
          aria-expanded={open || failed > 0}
          className="flex min-w-0 flex-1 items-center gap-2 rounded text-left focus-visible:outline-2 focus-visible:outline-accent-strong"
        >
          <ChevronRight
            className={`size-3.5 shrink-0 text-muted transition-transform ${open || failed > 0 ? 'rotate-90' : ''}`}
            aria-hidden
          />
          <span className="truncate text-ink">
            {active ? 'Uploading' : 'Uploaded'} {done} of {items.length}
            {failed > 0 && <span className="text-danger"> · {failed} failed</span>}
            {cancelled > 0 && <span className="text-muted"> · {cancelled} cancelled</span>}
          </span>
          <span className="hidden shrink-0 text-muted sm:inline">
            {humanSize(loaded)} of {humanSize(total)}
          </span>
        </button>
        <div
          className="h-1 w-24 shrink-0 overflow-hidden rounded-full bg-line"
          role="progressbar"
          aria-valuenow={pct}
          aria-valuemin={0}
          aria-valuemax={100}
        >
          <div
            className={`h-full transition-[width] ${failed > 0 ? 'bg-danger' : 'bg-accent'}`}
            style={{ width: `${pct}%` }}
          />
        </div>
        {active ? (
          <Button kind="quiet" size="sm" onClick={cancel}>
            Cancel
          </Button>
        ) : (
          <IconButton icon={X} size="sm" label="Clear the finished uploads" onClick={clear} />
        )}
      </div>
      {(open || failed > 0) && (
        <ul className="max-h-40 overflow-y-auto border-t border-line/60 py-1">
          {items.map((it) => (
            <li key={it.id} className="flex items-center gap-3 px-3 py-1 text-xs">
              <span className="min-w-0 flex-1 truncate font-mono text-ink-soft" title={`${it.bucket}/${it.key}`}>
                {it.key}
              </span>
              {it.state === 'failed' ? (
                <span className="max-w-[50%] truncate text-danger" title={it.error}>
                  {it.error}
                </span>
              ) : it.state === 'uploading' ? (
                <span className="shrink-0 text-muted">{it.size ? Math.round((it.loaded / it.size) * 100) : 0}%</span>
              ) : (
                <span className="shrink-0 text-muted">{it.state === 'done' ? humanSize(it.size) : it.state}</span>
              )}
            </li>
          ))}
        </ul>
      )}
    </div>
  )
}

function DeleteObjects({
  space,
  service,
  deletion,
  onClose,
  onDeleted,
}: {
  space: string
  service: string
  deletion: Deletion
  onClose: () => void
  onDeleted: () => void
}) {
  const { deleteMany } = useStoreWrites(space, service)
  const { keys, prefixes } = deletion
  const count = keys.length + prefixes.length
  const names = [...prefixes.map((p) => `${lastSegment(p)}/`), ...keys.map(lastSegment)]
  const busy = deleteMany.isPending
  return (
    <Modal onClose={() => !busy && onClose()} size="sm">
      <ModalHeader
        eyebrow={deletion.bucket}
        title={
          count === 1 ? (
            <>
              Delete <span className="font-mono">{names[0]}</span>?
            </>
          ) : (
            `Delete ${count} items?`
          )
        }
        onClose={onClose}
        closeDisabled={busy}
      />
      <ModalBody>
        <p className="text-sm leading-relaxed text-ink-soft">
          {prefixes.length > 0
            ? `${prefixes.length === 1 && count === 1 ? 'The folder goes' : 'Folders go'} with everything in them. Objects do not come back.`
            : 'Objects do not come back.'}
        </p>
        {count > 1 && (
          <ul className="mt-3 max-h-40 space-y-0.5 overflow-y-auto rounded-md bg-well p-2 font-mono text-xs text-ink-soft">
            {names.slice(0, 50).map((n) => (
              <li key={n} className="truncate">
                {n}
              </li>
            ))}
            {names.length > 50 && <li className="text-muted">and {names.length - 50} more</li>}
          </ul>
        )}
        {deleteMany.error && (
          <div className="mt-3">
            <ErrorNote message={mutationError(deleteMany, 'Deleting did not work.')} />
          </div>
        )}
      </ModalBody>
      <ModalFooter>
        <Button kind="quiet" onClick={onClose} disabled={busy}>
          Keep {count === 1 ? 'it' : 'them'}
        </Button>
        <Button
          kind="danger"
          icon={Trash2}
          disabled={busy}
          onClick={() => deleteMany.mutate(deletion, { onSuccess: onDeleted })}
        >
          {busy ? 'Deleting…' : 'Delete'}
        </Button>
      </ModalFooter>
    </Modal>
  )
}

// S3 replaces an object of the same name without a word; the console asks.
function ReplaceUploads({
  conflicts,
  skippable,
  onReplace,
  onSkip,
  onClose,
}: {
  conflicts: string[]
  skippable: boolean
  onReplace: () => void
  onSkip: () => void
  onClose: () => void
}) {
  return (
    <Modal onClose={onClose} size="sm">
      <ModalHeader
        title={
          conflicts.length === 1 ? (
            <>
              Replace <span className="font-mono">{lastSegment(conflicts[0])}</span>?
            </>
          ) : (
            `Replace ${conflicts.length} objects?`
          )
        }
        onClose={onClose}
      />
      <ModalBody>
        <p className="text-sm leading-relaxed text-ink-soft">
          {conflicts.length === 1 ? 'An object of that name is' : 'Objects of these names are'} already in this folder.
          Uploading replaces {conflicts.length === 1 ? 'it' : 'them'}.
        </p>
        {conflicts.length > 1 && (
          <ul className="mt-3 max-h-40 space-y-0.5 overflow-y-auto rounded-md bg-well p-2 font-mono text-xs text-ink-soft">
            {conflicts.map((c) => (
              <li key={c} className="truncate">
                {c}
              </li>
            ))}
          </ul>
        )}
      </ModalBody>
      <ModalFooter>
        <Button kind="quiet" onClick={onClose}>
          Cancel
        </Button>
        {skippable && (
          <Button kind="quiet" onClick={onSkip}>
            Upload the others
          </Button>
        )}
        <Button onClick={onReplace}>Replace</Button>
      </ModalFooter>
    </Modal>
  )
}

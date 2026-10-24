// The bucket browser's calls and their caches, the upload queue, and the
// way a selection becomes one download. Folders are nothing but key
// prefixes, the way S3 has always pretended; the server lists one level at
// a time and hands back S3's own continuation token for the next page.
import { useEffect, useRef, useState } from 'react'
import {
  infiniteQueryOptions,
  queryOptions,
  useInfiniteQuery,
  useMutation,
  useQuery,
  useQueryClient,
} from '@tanstack/react-query'
import { zipSync } from 'fflate'
import {
  FileArchive,
  FileAudio,
  FileBraces,
  FileCode,
  FileImage,
  FileSpreadsheet,
  FileText,
  FileType,
  FileVideo,
  File as FileIcon,
  type LucideIcon,
} from 'lucide-react'
import { api, ApiError, fetchOK, request, saveBlob, servicePath } from './api'
import { errText } from './format'

export interface S3Bucket {
  name: string
  createdAt: string
}

export interface S3Object {
  key: string
  size: number
  modified: string
  etag?: string
  storageClass?: string
}

export interface S3Listing {
  prefixes: string[]
  objects: S3Object[]
  truncated: boolean
  /** The token for the next page, or '' after the last. */
  next: string
}

/** The preview pane's object: its metadata, and its start as text when it is text. */
export interface S3Info {
  key: string
  size: number
  modified: string
  contentType: string
  etag: string
  storageClass: string
  versionId: string
  metadata: Record<string, string>
  text?: string
  truncated?: boolean
}

// ─── Names ─────────────────────────────────────────────────────────────

/** The last segment of a key or prefix — what a person recognizes. */
export const lastSegment = (key: string) => key.replace(/\/$/, '').split('/').pop() ?? key

const extension = (key: string) => (/\.([^./]+)$/.exec(key)?.[1] ?? '').toLowerCase()

// What a file is, from its name — the listing carries no content type (S3
// returns none), so this is the Finder's "kind", not the stored type.
const kinds: { label: string; icon: LucideIcon; ext: string[] }[] = [
  { label: 'Image', icon: FileImage, ext: ['png', 'jpg', 'jpeg', 'gif', 'webp', 'avif', 'svg', 'ico', 'bmp', 'heic', 'tif', 'tiff'] },
  { label: 'Markdown', icon: FileText, ext: ['md', 'markdown', 'mdx'] },
  { label: 'JSON', icon: FileBraces, ext: ['json', 'jsonl', 'ndjson', 'geojson', 'map'] },
  { label: 'Text', icon: FileText, ext: ['txt', 'log', 'text', 'rst'] },
  { label: 'Table', icon: FileSpreadsheet, ext: ['csv', 'tsv', 'xlsx', 'xls', 'ods', 'parquet', 'avro', 'orc', 'arrow', 'feather'] },
  { label: 'Config', icon: FileCode, ext: ['yaml', 'yml', 'toml', 'ini', 'env', 'conf', 'cfg', 'xml', 'properties'] },
  {
    label: 'Code',
    icon: FileCode,
    ext: ['js', 'mjs', 'cjs', 'ts', 'tsx', 'jsx', 'py', 'go', 'rb', 'rs', 'java', 'kt', 'c', 'h', 'cc', 'cpp', 'cs', 'php', 'sh', 'sql', 'css', 'scss', 'html', 'htm', 'vue', 'svelte'],
  },
  { label: 'Archive', icon: FileArchive, ext: ['zip', 'tar', 'gz', 'tgz', 'bz2', 'xz', '7z', 'zst', 'rar'] },
  { label: 'PDF', icon: FileText, ext: ['pdf'] },
  { label: 'Video', icon: FileVideo, ext: ['mp4', 'mov', 'webm', 'mkv', 'avi', 'm4v'] },
  { label: 'Audio', icon: FileAudio, ext: ['mp3', 'wav', 'ogg', 'flac', 'm4a', 'aac', 'opus'] },
  { label: 'Font', icon: FileType, ext: ['woff', 'woff2', 'ttf', 'otf'] },
]

export function kindOf(key: string): { label: string; icon: LucideIcon } {
  const ext = extension(key)
  return kinds.find((k) => k.ext.includes(ext)) ?? { label: ext ? ext.toUpperCase() : 'File', icon: FileIcon }
}

// The stored type decides, unless it is the "some bytes" a type-less upload
// gets — then the name does.
export function isImage(key: string, contentType = ''): boolean {
  if (contentType.startsWith('image/')) return true
  const generic = contentType === '' || /^(application|binary)\/octet-stream\b/.test(contentType)
  return generic && kindOf(key).label === 'Image'
}

export const isMarkdown = (key: string) => kindOf(key).label === 'Markdown'
export const isJsonName = (key: string, contentType?: string) =>
  kindOf(key).label === 'JSON' || /[/+]json\b/.test(contentType ?? '')

// ─── Calls ─────────────────────────────────────────────────────────────

const bucketPath = (space: string, service: string, bucket: string, suffix = '') =>
  servicePath(space, service, `/buckets/${encodeURIComponent(bucket)}${suffix}`)

/** Where an object downloads from — for a fetch, or an <img> on the same origin. */
export const s3DownloadUrl = (space: string, service: string, bucket: string, key: string, version = '') =>
  bucketPath(space, service, bucket, `/objects/download?${new URLSearchParams({ key, ...(version ? { v: version } : {}) })}`)

export const s3Api = {
  buckets: async (space: string, service: string) =>
    (await request<{ buckets: S3Bucket[] | null }>(servicePath(space, service, '/buckets'))).buckets ?? [],
  createBucket: (space: string, service: string, name: string) =>
    request<{ ok: boolean; name: string }>(servicePath(space, service, '/buckets'), {
      method: 'POST',
      body: JSON.stringify({ name }),
    }),
  deleteBucket: (space: string, service: string, name: string, force: boolean) =>
    request<{ ok: boolean }>(bucketPath(space, service, name, force ? '?force=true' : ''), { method: 'DELETE' }),
  list: async (
    space: string,
    service: string,
    bucket: string,
    prefix: string,
    token: string,
    opts: { recursive?: boolean; max?: number; signal?: AbortSignal } = {},
  ): Promise<S3Listing> => {
    const query = new URLSearchParams({ prefix })
    if (token) query.set('token', token)
    if (opts.recursive) query.set('recursive', 'true')
    if (opts.max) query.set('max', String(opts.max))
    const page = await request<S3Listing>(bucketPath(space, service, bucket, `/objects?${query}`), { signal: opts.signal })
    return { prefixes: page.prefixes ?? [], objects: page.objects ?? [], truncated: page.truncated, next: page.next ?? '' }
  },
  info: (space: string, service: string, bucket: string, key: string, signal?: AbortSignal) =>
    request<S3Info>(bucketPath(space, service, bucket, `/objects/info?${new URLSearchParams({ key })}`), { signal }),
  deleteMany: (space: string, service: string, bucket: string, keys: string[], prefixes: string[]) =>
    request<{ ok: boolean; deleted: number }>(bucketPath(space, service, bucket, '/objects/delete'), {
      method: 'POST',
      body: JSON.stringify({ keys, prefixes }),
    }),
  createFolder: (space: string, service: string, bucket: string, key: string) =>
    request<{ ok: boolean; key: string }>(bucketPath(space, service, bucket, '/folders'), {
      method: 'POST',
      body: JSON.stringify({ key }),
    }),
  download: async (space: string, service: string, bucket: string, key: string) =>
    (await fetchOK(s3DownloadUrl(space, service, bucket, key))).blob(),
}

// fetch cannot report upload progress, so uploads go over XMLHttpRequest.
// It rides the same session cookie; a 401 is handed to the shared client by
// a ping, which runs the console's usual sign-in-again path.
export function uploadObject(
  space: string,
  service: string,
  bucket: string,
  key: string,
  file: File,
  onProgress: (loaded: number) => void,
  signal: AbortSignal,
): Promise<void> {
  return new Promise((resolve, reject) => {
    const xhr = new XMLHttpRequest()
    xhr.open('POST', bucketPath(space, service, bucket, `/objects?${new URLSearchParams({ key })}`))
    xhr.responseType = 'json'
    xhr.upload.onprogress = (e) => onProgress(Math.min(e.loaded, file.size))
    xhr.onload = () => {
      if (xhr.status >= 200 && xhr.status < 300) return resolve()
      if (xhr.status === 401) void api.ping().catch(() => {})
      const message = (xhr.response as { error?: string } | null)?.error
      reject(new ApiError(xhr.status, message ?? `The server answered HTTP ${xhr.status}`))
    }
    xhr.onerror = () => reject(new Error('The upload did not reach the server.'))
    xhr.onabort = () => reject(new DOMException('The upload was cancelled.', 'AbortError'))
    signal.addEventListener('abort', () => xhr.abort(), { once: true })
    const body = new FormData()
    body.append('file', file)
    xhr.send(body)
  })
}

// ─── Queries ───────────────────────────────────────────────────────────

const root = (space: string, service: string) => ['s3', space, service] as const

export const s3BucketsQuery = (space: string, service: string) =>
  queryOptions({ queryKey: [...root(space, service), 'buckets'], queryFn: () => s3Api.buckets(space, service) })

export const s3ObjectsQuery = (space: string, service: string, bucket: string, prefix: string) =>
  infiniteQueryOptions({
    queryKey: [...root(space, service), 'objects', bucket, prefix],
    queryFn: ({ pageParam, signal }) => s3Api.list(space, service, bucket, prefix, pageParam, { signal }),
    initialPageParam: '',
    getNextPageParam: (last) => last.next || undefined,
  })

export const s3InfoQuery = (space: string, service: string, bucket: string, key: string) =>
  queryOptions({
    queryKey: [...root(space, service), 'info', bucket, key],
    queryFn: ({ signal }) => s3Api.info(space, service, bucket, key, signal),
    retry: (count, err) => (err as { status?: number }).status !== 404 && count < 2,
  })

export function useS3Buckets(space: string, service: string) {
  return useQuery(s3BucketsQuery(space, service))
}

export function useS3Objects(space: string, service: string, bucket: string, prefix: string) {
  return useInfiniteQuery({ ...s3ObjectsQuery(space, service, bucket, prefix), enabled: bucket !== '' })
}

export function useS3Info(space: string, service: string, bucket: string, key: string) {
  return useQuery(s3InfoQuery(space, service, bucket, key))
}

/** The writes, each refreshing what it changed. */
export function useS3Writes(space: string, service: string) {
  const qc = useQueryClient()
  const buckets = () => qc.invalidateQueries({ queryKey: s3BucketsQuery(space, service).queryKey })
  const objects = (bucket: string) => qc.invalidateQueries({ queryKey: [...root(space, service), 'objects', bucket] })

  const createBucket = useMutation({
    mutationFn: (name: string) => s3Api.createBucket(space, service, name),
    onSuccess: () => buckets(),
  })
  const deleteBucket = useMutation({
    mutationFn: ({ name, force }: { name: string; force: boolean }) => s3Api.deleteBucket(space, service, name, force),
    onSuccess: (_, { name }) => {
      qc.removeQueries({ queryKey: [...root(space, service), 'objects', name] })
      qc.removeQueries({ queryKey: [...root(space, service), 'info', name] })
      return buckets()
    },
  })
  const createFolder = useMutation({
    mutationFn: ({ bucket, key }: { bucket: string; key: string }) => s3Api.createFolder(space, service, bucket, key),
    onSuccess: (_, { bucket }) => objects(bucket),
  })
  // The server takes a thousand keys or a hundred folders a request.
  const deleteMany = useMutation({
    mutationFn: async ({ bucket, keys, prefixes }: { bucket: string; keys: string[]; prefixes: string[] }) => {
      let deleted = 0
      for (let i = 0; i < keys.length; i += 1000) {
        deleted += (await s3Api.deleteMany(space, service, bucket, keys.slice(i, i + 1000), [])).deleted
      }
      for (let i = 0; i < prefixes.length; i += 100) {
        deleted += (await s3Api.deleteMany(space, service, bucket, [], prefixes.slice(i, i + 100))).deleted
      }
      return deleted
    },
    onSettled: (_, __, { bucket }) => {
      // An open preview of something deleted reads again, and finds it gone.
      qc.invalidateQueries({ queryKey: [...root(space, service), 'info', bucket] })
      return objects(bucket)
    },
  })
  return { createBucket, deleteBucket, createFolder, deleteMany, refreshObjects: objects }
}

// ─── Uploads ───────────────────────────────────────────────────────────

export type UploadItem = {
  id: number
  bucket: string
  key: string
  size: number
  loaded: number
  state: 'queued' | 'uploading' | 'done' | 'failed' | 'cancelled'
  error?: string
}

/** A file picked or dropped, and the path below the folder it goes to. */
export type UploadSource = { file: File; path: string }

const parallel = 3

/**
 * The upload queue: a few at a time, each with its progress. It lives with
 * the console, not a folder, so walking elsewhere keeps uploads going; and
 * while any runs, leaving the page asks first.
 */
export function useUploads(space: string, service: string, onUploaded: (bucket: string) => void) {
  const [items, setItems] = useState<UploadItem[]>([])
  const queue = useRef<{ item: UploadItem; file: File; controller: AbortController }[]>([])
  const running = useRef(0)
  const nextId = useRef(1)
  const controllers = useRef(new Map<number, AbortController>())
  const uploaded = useRef(onUploaded)
  useEffect(() => {
    uploaded.current = onUploaded
  })
  // A listing refresh per finished file would rescan the folder a hundred
  // times for a hundred files; one a second is plenty.
  const refresh = useRef<{ timer?: number; buckets: Set<string> }>({ buckets: new Set() })

  const patch = (id: number, next: Partial<UploadItem>) =>
    setItems((cur) => cur.map((it) => (it.id === id ? { ...it, ...next } : it)))

  const scheduleRefresh = (bucket: string) => {
    refresh.current.buckets.add(bucket)
    if (refresh.current.timer !== undefined) return
    refresh.current.timer = window.setTimeout(() => {
      for (const b of refresh.current.buckets) uploaded.current(b)
      refresh.current = { buckets: new Set() }
    }, 1000)
  }

  const pump = () => {
    while (running.current < parallel && queue.current.length > 0) {
      const { item, file, controller } = queue.current.shift()!
      if (controller.signal.aborted) continue
      running.current++
      patch(item.id, { state: 'uploading' })
      uploadObject(space, service, item.bucket, item.key, file, (loaded) => patch(item.id, { loaded }), controller.signal)
        .then(
          () => {
            patch(item.id, { state: 'done', loaded: item.size })
            scheduleRefresh(item.bucket)
          },
          (err) =>
            patch(item.id, controller.signal.aborted ? { state: 'cancelled' } : { state: 'failed', error: errText(err, 'The upload did not work.') }),
        )
        .finally(() => {
          controllers.current.delete(item.id)
          running.current--
          pump()
        })
    }
  }

  const add = (bucket: string, prefix: string, sources: UploadSource[]) => {
    const added = sources.map(({ file, path }) => {
      const item: UploadItem = { id: nextId.current++, bucket, key: prefix + path, size: file.size, loaded: 0, state: 'queued' }
      const controller = new AbortController()
      controllers.current.set(item.id, controller)
      queue.current.push({ item, file, controller })
      return item
    })
    setItems((cur) => [...cur, ...added])
    pump()
  }

  const cancel = () => {
    for (const c of controllers.current.values()) c.abort()
    setItems((cur) => cur.map((it) => (it.state === 'queued' ? { ...it, state: 'cancelled' } : it)))
  }

  const clear = () => setItems((cur) => cur.filter((it) => it.state === 'queued' || it.state === 'uploading'))

  const active = items.some((it) => it.state === 'queued' || it.state === 'uploading')
  useEffect(() => {
    if (!active) return
    const warn = (e: BeforeUnloadEvent) => e.preventDefault()
    window.addEventListener('beforeunload', warn)
    return () => window.removeEventListener('beforeunload', warn)
  }, [active])

  return { items, add, cancel, clear, active }
}

// A drop's files, folders walked: a dropped folder lands as a folder of
// the same name, its files at their paths inside it.
export async function dropSources(transfer: DataTransfer): Promise<UploadSource[]> {
  const entries = Array.from(transfer.items)
    .map((item) => item.webkitGetAsEntry?.())
    .filter((e): e is FileSystemEntry => !!e)
  if (entries.length === 0) return Array.from(transfer.files).map((file) => ({ file, path: file.name }))
  const out: UploadSource[] = []
  const walk = async (entry: FileSystemEntry, at: string): Promise<void> => {
    if (entry.isFile) {
      const file = await new Promise<File>((resolve, reject) => (entry as FileSystemFileEntry).file(resolve, reject))
      out.push({ file, path: at + entry.name })
      return
    }
    const reader = (entry as FileSystemDirectoryEntry).createReader()
    for (;;) {
      const batch = await new Promise<FileSystemEntry[]>((resolve, reject) => reader.readEntries(resolve, reject))
      if (batch.length === 0) break
      for (const child of batch) await walk(child, `${at}${entry.name}/`)
    }
  }
  for (const entry of entries) await walk(entry, '')
  return out
}

// ─── Downloads ─────────────────────────────────────────────────────────

// A zip is built in the browser's memory: past these it is the CLI's job.
const zipMaxObjects = 2000
const zipMaxBytes = 512 << 20

/**
 * One file for a selection: an object as itself, anything more as a zip
 * of the objects with their paths below the folder being looked at. A
 * selected folder brings everything under it.
 */
export async function downloadSelection(
  space: string,
  service: string,
  bucket: string,
  prefix: string,
  keys: string[],
  folders: string[],
  onStatus: (status: string) => void,
) {
  if (keys.length === 1 && folders.length === 0) {
    onStatus(`Downloading ${lastSegment(keys[0])}…`)
    saveBlob(await s3Api.download(space, service, bucket, keys[0]), lastSegment(keys[0]))
    return
  }
  const objects = keys.map((key) => ({ key, size: 0 }))
  for (const folder of folders) {
    onStatus(`Listing ${lastSegment(folder)}/…`)
    let token = ''
    do {
      const page = await s3Api.list(space, service, bucket, folder, token, { recursive: true, max: 1000 })
      // Folder markers hold nothing a zip needs; the paths make the folders.
      objects.push(...page.objects.filter((o) => !o.key.endsWith('/')))
      if (objects.length > zipMaxObjects) throw new Error(`That is more than ${zipMaxObjects} objects — too many to zip in the browser; use the CLI or a tunnel.`)
      token = page.next
    } while (token)
  }
  if (objects.reduce((n, o) => n + o.size, 0) > zipMaxBytes) {
    throw new Error('That is more than 512 MB — too much to zip in the browser; use the CLI or a tunnel.')
  }
  const files: Record<string, [Uint8Array, { level: 0 }]> = {}
  let total = 0
  for (const [i, o] of objects.entries()) {
    onStatus(`Fetching ${i + 1} of ${objects.length}…`)
    const data = new Uint8Array(await (await s3Api.download(space, service, bucket, o.key)).arrayBuffer())
    total += data.length
    if (total > zipMaxBytes) throw new Error('That is more than 512 MB — too much to zip in the browser; use the CLI or a tunnel.')
    // Stored, not deflated: most of what sits in buckets is compressed already.
    files[o.key.slice(prefix.length)] = [data, { level: 0 }]
  }
  onStatus('Zipping…')
  const name = folders.length === 1 && keys.length === 0 ? lastSegment(folders[0]) : lastSegment(prefix) || bucket
  saveBlob(new Blob([zipSync(files)], { type: 'application/zip' }), `${name}.zip`)
}

// The selected object: a look at it — an image, or the start of a text file
// highlighted (markdown rendered, JSON re-spaced) — and what the store keeps
// about it. Anything else is metadata and a download.
import { useState, type ReactNode } from 'react'
import { Markdown } from '@tanstack/markdown/react'
import { Download, Trash2, X } from 'lucide-react'
import { prettyJson } from './redisApi'
import {
  isImage,
  isJsonName,
  isMarkdown,
  kindOf,
  lastSegment,
  s3Api,
  s3DownloadUrl,
  useS3Info,
  type S3Info,
} from './s3Api'
import { saveBlob } from './api'
import { SourceCode } from './SourceCode'
import { Button, CopyButton, ErrorNote, Eyebrow, IconButton, Select, Spinner } from './ui'
import { errText, humanSize } from './format'

// An image is shown up to this size; past it the preview would be a long
// wait for a thumbnail.
const imageMaxBytes = 20 << 20

// Rendered markdown in the console's tokens, scoped to the preview.
const markdownCss = `
.s3-md{font-size:13px;line-height:1.65;color:var(--color-ink-soft);overflow-wrap:anywhere}
.s3-md>:first-child{margin-top:0}
.s3-md h1,.s3-md h2,.s3-md h3,.s3-md h4{color:var(--color-ink);font-weight:600;margin:1.1em 0 .4em;line-height:1.3}
.s3-md h1{font-size:1.25rem}.s3-md h2{font-size:1.1rem}.s3-md h3{font-size:1rem}
.s3-md p,.s3-md ul,.s3-md ol,.s3-md pre,.s3-md table,.s3-md blockquote{margin:.55em 0}
.s3-md ul,.s3-md ol{padding-left:1.25rem;list-style:revert}
.s3-md a{color:var(--color-accent-strong);text-decoration:underline}
.s3-md code{font-family:var(--font-mono);font-size:.88em;background:var(--color-raised);padding:.1em .3em;border-radius:4px}
.s3-md pre{background:var(--color-well);padding:.75rem;border-radius:6px;overflow-x:auto}
.s3-md pre code{background:none;padding:0}
.s3-md blockquote{border-left:2px solid var(--color-line-strong);padding-left:.75rem;color:var(--color-muted)}
.s3-md table{border-collapse:collapse}
.s3-md th,.s3-md td{border:1px solid var(--color-line);padding:2px 8px}
.s3-md img{max-width:100%}
.s3-md hr{border:0;border-top:1px solid var(--color-line);margin:1em 0}
`

export function S3Preview({
  space,
  service,
  bucket,
  objectKey,
  onClose,
  onDelete,
  className = '',
}: {
  space: string
  service: string
  bucket: string
  objectKey: string
  onClose: () => void
  onDelete: () => void
  className?: string
}) {
  const { data: info, error, isPending } = useS3Info(space, service, bucket, objectKey)
  const [downloading, setDownloading] = useState(false)
  const [downloadError, setDownloadError] = useState('')
  const [dims, setDims] = useState('')
  const gone = (error as { status?: number } | null)?.status === 404
  const Icon = kindOf(objectKey).icon

  const download = async () => {
    setDownloading(true)
    setDownloadError('')
    try {
      saveBlob(await s3Api.download(space, service, bucket, objectKey), lastSegment(objectKey))
    } catch (err) {
      setDownloadError(errText(err, 'The download did not work.'))
    } finally {
      setDownloading(false)
    }
  }

  return (
    <aside className={`flex min-h-0 flex-col overflow-hidden ${className}`} aria-label="Object preview">
      <header className="flex min-h-11 shrink-0 items-center gap-2 border-b border-line py-1 pr-1.5 pl-3">
        <Icon className="size-4 shrink-0 text-muted" aria-hidden />
        <h3 className="min-w-0 flex-1 truncate font-mono text-sm font-semibold text-ink" title={objectKey}>
          {lastSegment(objectKey)}
        </h3>
        <IconButton icon={X} label="Close the preview" onClick={onClose} />
      </header>
      <div className="flex min-h-0 flex-1 flex-col">
        {gone ? (
          <p className="p-4 text-sm text-muted">This object is gone — it was deleted or replaced by a folder.</p>
        ) : error ? (
          <div className="p-3">
            <ErrorNote message={errText(error, 'Reading the object did not work.')} />
          </div>
        ) : isPending || !info ? (
          <Spinner label="Reading the object…" />
        ) : (
          <>
            <Look space={space} service={service} bucket={bucket} info={info} onDims={setDims} />
            <Facts bucket={bucket} info={info} dims={dims} />
          </>
        )}
      </div>
      {downloadError && (
        <div className="shrink-0 border-t border-line p-3">
          <ErrorNote message={downloadError} />
        </div>
      )}
      <footer className="flex shrink-0 items-center justify-between gap-2 border-t border-line py-2 pr-1.5 pl-3">
        <Button size="sm" icon={Download} disabled={downloading || gone} onClick={() => void download()}>
          {downloading ? 'Downloading…' : 'Download'}
        </Button>
        <IconButton icon={Trash2} kind="danger" label="Delete the object…" disabled={gone} onClick={onDelete} />
      </footer>
    </aside>
  )
}

function Look({
  space,
  service,
  bucket,
  info,
  onDims,
}: {
  space: string
  service: string
  bucket: string
  info: S3Info
  onDims: (dims: string) => void
}) {
  const [broken, setBroken] = useState(false)
  const [rendered, setRendered] = useState(true)
  if (info.size === 0) return <Note>An empty object.</Note>
  if (isImage(info.key, info.contentType)) {
    if (info.size > imageMaxBytes) return <Note>Too large to preview here — download it to look.</Note>
    if (broken) return <Note>The browser could not show this image.</Note>
    return (
      <div className="flex shrink-0 items-center justify-center border-b border-line bg-well p-3">
        {/* Same origin, so the session rides along; the ETag busts the
            cache when the object is replaced under the same key. */}
        <img
          src={s3DownloadUrl(space, service, bucket, info.key, info.etag)}
          alt={lastSegment(info.key)}
          onLoad={(e) => onDims(`${e.currentTarget.naturalWidth} × ${e.currentTarget.naturalHeight}`)}
          onError={() => setBroken(true)}
          className="max-h-80 max-w-full object-contain"
        />
      </div>
    )
  }
  if (info.text === undefined) return <Note>No preview for this kind of file — download it to open it.</Note>

  const markdown = isMarkdown(info.key)
  // Newline-delimited JSON stays one record a line.
  const json =
    isJsonName(info.key, info.contentType) && !/\.(jsonl|ndjson)$/i.test(info.key) && /^\s*[[{]/.test(info.text)
  return (
    <div className="flex min-h-0 flex-1 flex-col border-b border-line">
      {(markdown || info.truncated) && (
        <div className="flex min-h-9 shrink-0 items-center justify-between gap-2 border-b border-line pr-1.5 pl-3 text-xs text-muted">
          <span>
            {info.truncated
              ? `The first ${humanSize(new TextEncoder().encode(info.text).length)} of ${humanSize(info.size)}`
              : 'Markdown'}
          </span>
          {markdown && (
            <Select
              variant="quiet"
              ariaLabel="Show as"
              value={rendered ? 'rendered' : 'source'}
              onChange={(v) => setRendered(v === 'rendered')}
              options={[
                { value: 'rendered', label: 'Rendered' },
                { value: 'source', label: 'Source' },
              ]}
            />
          )}
        </div>
      )}
      {markdown && rendered ? (
        <div className="s3-md min-h-0 flex-1 overflow-auto px-4 py-3">
          <style>{markdownCss}</style>
          <Markdown>{info.text}</Markdown>
        </div>
      ) : (
        <div className="min-h-0 flex-1 overflow-auto bg-well">
          <SourceCode
            path={json ? 'object.json' : info.key}
            text={json ? prettyJson(info.text) : info.text}
            className=""
          />
        </div>
      )}
    </div>
  )
}

function Note({ children }: { children: ReactNode }) {
  return <p className="shrink-0 border-b border-line px-4 py-6 text-center text-sm text-muted">{children}</p>
}

function Facts({ bucket, info, dims }: { bucket: string; info: S3Info; dims: string }) {
  const meta = Object.entries(info.metadata ?? {})
  return (
    <dl
      className={`shrink-0 space-y-2.5 overflow-y-auto p-3 ${info.text !== undefined && info.size > 0 ? 'max-h-[45%]' : 'flex-1'}`}
    >
      <Fact label="Key" copy={info.key}>
        {info.key}
      </Fact>
      <Fact label="Address" copy={`s3://${bucket}/${info.key}`}>
        s3://{bucket}/{info.key}
      </Fact>
      <div className="grid grid-cols-2 gap-x-3 gap-y-2.5">
        <Fact label="Size">
          {humanSize(info.size)}
          {info.size >= 1024 && <span className="text-muted"> · {info.size.toLocaleString()} B</span>}
        </Fact>
        <Fact label="Modified">
          <span title={info.modified}>{new Date(info.modified).toLocaleString()}</span>
        </Fact>
        <Fact label="Content type">{info.contentType || '—'}</Fact>
        {dims && <Fact label="Dimensions">{dims}</Fact>}
        {info.storageClass && <Fact label="Storage class">{info.storageClass}</Fact>}
      </div>
      {info.etag && <Fact label="ETag">{info.etag}</Fact>}
      {info.versionId && <Fact label="Version">{info.versionId}</Fact>}
      {meta.length > 0 && (
        <div>
          <dt>
            <Eyebrow>Metadata</Eyebrow>
          </dt>
          <dd className="mt-1 space-y-0.5">
            {meta.map(([k, v]) => (
              <div key={k} className="flex gap-2 font-mono text-xs">
                <span className="shrink-0 text-muted">{k.toLowerCase()}</span>
                <span className="min-w-0 break-all text-ink">{v}</span>
              </div>
            ))}
          </dd>
        </div>
      )}
    </dl>
  )
}

function Fact({ label, copy, children }: { label: string; copy?: string; children: ReactNode }) {
  return (
    <div className="min-w-0">
      <dt>
        <Eyebrow>{label}</Eyebrow>
      </dt>
      <dd className="mt-0.5 flex min-w-0 items-start gap-1">
        <span className="min-w-0 flex-1 break-all font-mono text-xs leading-5 text-ink">{children}</span>
        {copy !== undefined && (
          <span className="-my-1.5 -mr-1.5 shrink-0">
            <CopyButton text={copy} label={`Copy the ${label.toLowerCase()}`} />
          </span>
        )}
      </dd>
    </div>
  )
}

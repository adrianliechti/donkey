import { useState } from 'react'
import { CircleSlash, Download, Send, X } from 'lucide-react'
import { saveBlob } from './api'
import {
  bodyLanguage,
  decodeBase64,
  fullURL,
  headerValue,
  methodTone,
  prettyJSON,
  statusTone,
  toRequest,
  type Exchange,
  type HttpResult,
} from './httpApi'
import { Keys, Pane, PaneEmpty, PaneHeader, PaneTabs, Segmented } from './Workbench'
import { useNow } from './hooks'
import { SourceCode } from './SourceCode'
import { Button, Callout, CopyButton, ErrorNote, IconButton, Spinner } from './ui'
import { humanSize } from './format'

// The answer pane of the HTTP console: the body shown as what it is —
// highlighted JSON or markup, an image, a binary to save — and the headers
// that came back beside the ones that went out.

type ResponseTab = 'body' | 'headers'

export function ResponsePane({
  service,
  base,
  exchange,
  onCancel,
}: {
  service: string
  base: string
  exchange?: Exchange
  onCancel: () => void
}) {
  const [tab, setTab] = useState<ResponseTab>('body')
  const [raw, setRaw] = useState(false)
  const result = exchange?.state === 'done' ? exchange.result : undefined
  const contentType = result ? headerValue(result.headers, 'Content-Type') : ''
  const binary = result?.encoding === 'base64'
  const language = result && !binary ? bodyLanguage(contentType, result.body) : ''

  return (
    <Pane label="Response">
      <PaneHeader
        flush
        actions={
          result && (
            <>
              <StatusMeta result={result} />
              {tab === 'body' && language && (
                <Segmented
                  label="Body view"
                  value={raw ? 'raw' : 'pretty'}
                  onChange={(v) => setRaw(v === 'raw')}
                  options={[
                    { value: 'pretty', label: 'Pretty' },
                    { value: 'raw', label: 'Raw' },
                  ]}
                />
              )}
              {result.body && !binary && <CopyButton text={result.body} label="Copy the body" />}
              {result.body && (
                <IconButton
                  icon={Download}
                  label="Download the body"
                  onClick={() => download(result, exchange?.draft.path ?? '', contentType)}
                />
              )}
            </>
          )
        }
      >
        <PaneTabs
          label="Response"
          value={tab}
          onChange={setTab}
          tabs={[
            { id: 'body', label: 'Body' },
            { id: 'headers', label: 'Headers', count: result ? Object.keys(result.headers).length : undefined },
          ]}
        />
      </PaneHeader>
      <div className="flex min-h-0 flex-1 flex-col overflow-auto">
        {!exchange && (
          <PaneEmpty icon={Send} title="No answer yet">
            <p>
              Send the request with <Keys chord="send" />. It goes to{' '}
              <span className="font-mono break-all text-ink-soft">{base || service}</span> through donkey's proxy, which
              adds the service's credential.
            </p>
          </PaneEmpty>
        )}
        {exchange?.state === 'pending' && <Waiting service={service} since={exchange.startedAt} onCancel={onCancel} />}
        {exchange?.state === 'cancelled' && (
          <PaneEmpty icon={X} title="Cancelled">
            The request was cancelled before an answer came.
          </PaneEmpty>
        )}
        {exchange?.state === 'failed' && (
          <div className="p-3">
            <ErrorNote message={exchange.error ?? 'The request did not work.'} />
          </div>
        )}
        {result && tab === 'body' && (
          <BodyView
            result={result}
            contentType={contentType}
            language={language}
            raw={raw}
            method={exchange?.draft.method ?? ''}
          />
        )}
        {result && tab === 'headers' && <HeadersView result={result} exchange={exchange} base={base} />}
      </div>
    </Pane>
  )
}

function StatusMeta({ result }: { result: HttpResult }) {
  return (
    <span aria-live="polite" className="mr-1 flex items-center gap-2.5 font-mono text-xs text-muted">
      <span className={`rounded px-1.5 py-0.5 font-medium ${statusTone(result.status)}`}>
        {result.status} {result.statusText}
      </span>
      <span title="Time to the answer">{result.durationMs} ms</span>
      {result.size !== undefined && (
        <span title={result.truncated ? 'Larger than the 1 MiB shown' : 'Body size'}>
          {humanSize(result.size)}
          {result.truncated && '+'}
        </span>
      )}
    </span>
  )
}

function Waiting({ service, since, onCancel }: { service: string; since: number; onCancel: () => void }) {
  const now = useNow(250)
  return (
    <div className="flex flex-1 flex-col items-center justify-center gap-3">
      <Spinner inline label={`Waiting for ${service}… ${((now - since) / 1000).toFixed(1)} s`} />
      <Button kind="quiet" size="sm" icon={X} onClick={onCancel}>
        Cancel
      </Button>
    </div>
  )
}

// Highlighting lays out a table row per line; past this a body is shown
// plain, which the browser renders at once.
const maxHighlighted = 200_000

function BodyView({
  result,
  contentType,
  language,
  raw,
  method,
}: {
  result: HttpResult
  contentType: string
  language: string
  raw: boolean
  method: string
}) {
  const location = result.status >= 300 && result.status < 400 ? headerValue(result.headers, 'Location') : ''
  const notes = (
    <>
      {location && (
        <Callout tone="info" className="m-3 mb-0">
          Redirects to <span className="font-mono break-all">{location}</span> — the console shows the redirect rather
          than following it.
        </Callout>
      )}
      {result.truncated && (
        <Callout className="m-3 mb-0">Showing the first {humanSize(result.size ?? 1 << 20)} of a larger body.</Callout>
      )}
    </>
  )

  if (!result.body) {
    return (
      <>
        {notes}
        <PaneEmpty icon={CircleSlash} title="No body">
          {method === 'HEAD' ? 'HEAD answers carry headers only.' : 'The service answered without a body.'}
        </PaneEmpty>
      </>
    )
  }

  if (result.encoding === 'base64') {
    const image = /^image\/(png|jpe?g|gif|webp|avif|bmp|x-icon|vnd\.microsoft\.icon)/i.test(contentType)
    return (
      <>
        {notes}
        {image && !result.truncated ? (
          <div className="flex flex-1 items-center justify-center bg-well p-4">
            <img
              src={`data:${contentType.split(';')[0]};base64,${result.body}`}
              alt="The image the service answered with"
              className="max-h-full max-w-full object-contain"
            />
          </div>
        ) : (
          <PaneEmpty icon={Download} title="A binary body">
            {humanSize(result.size ?? 0)} of {contentType || 'unnamed binary data'} — download it to open it.
          </PaneEmpty>
        )}
      </>
    )
  }

  const pretty = !raw && language === 'json' ? prettyJSON(result.body) : result.body
  const highlighted = !raw && language && pretty.length <= maxHighlighted
  return (
    <>
      {notes}
      {highlighted ? (
        <SourceCode path={`body.${language}`} text={pretty} className="flex-1" />
      ) : (
        <pre className="flex-1 bg-well p-3 font-mono text-xs leading-relaxed break-all whitespace-pre-wrap text-emphasis">
          {pretty}
        </pre>
      )}
    </>
  )
}

function HeadersView({ result, exchange, base }: { result: HttpResult; exchange?: Exchange; base: string }) {
  const received = Object.entries(result.headers).flatMap(([name, values]) => values.map((value) => [name, value]))
  const sent = exchange ? Object.entries(toRequest(exchange.draft).headers) : []
  const url = result.url || (exchange ? fullURL(base, toRequest(exchange.draft).url) : '')
  return (
    <div className="space-y-4 p-3">
      {url && (
        <p className="font-mono text-xs break-all text-ink-soft">
          <span className={`font-semibold ${methodTone(exchange?.draft.method ?? '')}`}>{exchange?.draft.method}</span>{' '}
          {url}
        </p>
      )}
      <HeaderList title="Received" rows={received} copy />
      <HeaderList title="Sent" rows={sent} note="Plus the service's credential, which donkey's proxy adds." />
    </div>
  )
}

function HeaderList({
  title,
  rows,
  note,
  copy = false,
}: {
  title: string
  rows: string[][]
  note?: string
  copy?: boolean
}) {
  return (
    <section className="space-y-1.5">
      <div className="flex items-center justify-between">
        <h3 className="font-mono text-[10px] font-semibold tracking-[0.14em] text-muted uppercase">{title}</h3>
        {copy && rows.length > 0 && (
          <CopyButton
            text={rows.map(([n, v]) => `${n}: ${v}`).join('\n')}
            label={`Copy the ${title.toLowerCase()} headers`}
          />
        )}
      </div>
      {rows.length === 0 ? (
        <p className="text-xs text-muted">None.</p>
      ) : (
        <dl className="grid grid-cols-[minmax(8rem,14rem)_minmax(0,1fr)] gap-x-4 gap-y-1 font-mono text-xs">
          {rows.map(([name, value], i) => (
            <div key={`${name}:${i}`} className="contents">
              <dt className="truncate text-ink" title={name}>
                {name}
              </dt>
              <dd className="break-all text-ink-soft">{value}</dd>
            </div>
          ))}
        </dl>
      )}
      {note && <p className="text-xs text-muted">{note}</p>}
    </section>
  )
}

const extensions: Record<string, string> = { json: 'json', html: 'html', css: 'css', js: 'js', yaml: 'yaml' }

function download(result: HttpResult, path: string, contentType: string) {
  const data = result.encoding === 'base64' ? decodeBase64(result.body) : result.body
  const last = path.split('?')[0].split('/').filter(Boolean).pop() ?? ''
  const language = result.encoding === 'base64' ? '' : bodyLanguage(contentType, result.body)
  const name = last.includes('.') ? last : `${last || 'response'}${language ? `.${extensions[language]}` : ''}`
  saveBlob(new Blob([data], { type: contentType.split(';')[0] || 'application/octet-stream' }), name)
}

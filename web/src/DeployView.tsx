import { useRef, useState } from 'react'
import { CheckCircle2, ExternalLink, FileArchive, FolderUp, UploadCloud } from 'lucide-react'
import { zipSync } from 'fflate'
import { api, type DeployResult } from './api'
import { Button, Card, ErrorNote, errText, SectionTitle } from './ui'
import { DiffView } from './DiffView'

// The same exclusions the CLI applies while packing — a browser upload
// should not ship what `donkey deploy` would leave behind.
const SKIP_DIRS = new Set(['.git', 'node_modules', '.venv', 'venv', '__pycache__', '.idea', '.vscode'])
const SENSITIVE_EXT = ['.pem', '.key', '.p12', '.pfx']
const SSH_KEYS = new Set(['id_rsa', 'id_dsa', 'id_ecdsa', 'id_ed25519'])

function sensitiveFile(name: string): boolean {
  const lower = name.toLowerCase()
  if (lower.startsWith('.env')) return true
  if (SENSITIVE_EXT.some((ext) => lower.endsWith(ext))) return true
  return SSH_KEYS.has(lower) || SSH_KEYS.has(lower.replace(/\.pub$/, ''))
}

const MAX_UPLOAD = 256 * 1024 * 1024

interface Collected {
  path: string
  file: File
}

async function collectEntry(
  entry: FileSystemEntry,
  prefix: string,
  out: Collected[],
  skipped: string[],
): Promise<void> {
  if (entry.isDirectory) {
    if (SKIP_DIRS.has(entry.name)) {
      skipped.push(prefix + entry.name + '/')
      return
    }
    const reader = (entry as FileSystemDirectoryEntry).createReader()
    for (;;) {
      const batch = await new Promise<FileSystemEntry[]>((resolve, reject) =>
        reader.readEntries(resolve, reject),
      )
      if (batch.length === 0) break
      for (const e of batch) await collectEntry(e, prefix + entry.name + '/', out, skipped)
    }
    return
  }
  if (sensitiveFile(entry.name)) {
    skipped.push(prefix + entry.name)
    return
  }
  const file = await new Promise<File>((resolve, reject) =>
    (entry as FileSystemFileEntry).file(resolve, reject),
  )
  out.push({ path: prefix + entry.name, file })
}

// Files that mark a folder as "an app lives here" when no donkey.yaml does.
const APP_MARKERS = new Set([
  'package.json',
  'go.mod',
  'requirements.txt',
  'pyproject.toml',
  'Pipfile',
  'Gemfile',
  'composer.json',
  'pom.xml',
  'build.gradle',
  'Cargo.toml',
  'mix.exs',
  'Dockerfile',
  'index.html',
])

// candidateRoots finds the folders that look like an app, nearest first. The
// browser knows the whole file list before anything is uploaded, so a
// monorepo can be narrowed down here instead of failing server-side.
function candidateRoots(paths: string[]): { dir: string; hasConfig: boolean }[] {
  const configDirs = new Set<string>()
  const markerDirs = new Set<string>()
  for (const p of paths) {
    const slash = p.lastIndexOf('/')
    const dir = slash < 0 ? '' : p.slice(0, slash)
    const base = slash < 0 ? p : p.slice(slash + 1)
    if (base === 'donkey.yaml') configDirs.add(dir)
    else if (APP_MARKERS.has(base)) markerDirs.add(dir)
  }
  const dirs = configDirs.size > 0 ? configDirs : markerDirs
  return [...dirs]
    .map((dir) => ({ dir, hasConfig: configDirs.has(dir) }))
    .sort((a, b) => a.dir.split('/').length - b.dir.split('/').length || a.dir.localeCompare(b.dir))
}

// Upload is what the drop zone produced: the collected files plus what we
// know about them. A ready-made .zip is passed through unopened.
interface Upload {
  label: string
  files: Collected[] | null // null = an opaque .zip, sent as-is
  blob: Blob | null
  roots: { dir: string; hasConfig: boolean }[]
  skipped: string[]
}

// zipFrom packs the chosen folder, rebased so the app sits at the zip root.
function zipFrom(files: Collected[], root: string, buffers: Map<string, Uint8Array>): Blob {
  const prefix = root === '' ? '' : root + '/'
  const data: Record<string, Uint8Array> = {}
  for (const f of files) {
    if (!f.path.startsWith(prefix)) continue
    const buf = buffers.get(f.path)
    if (buf) data[f.path.slice(prefix.length)] = buf
  }
  return new Blob([zipSync(data)], { type: 'application/zip' })
}

async function uploadFromDrop(
  items: DataTransferItemList,
  buffers: Map<string, Uint8Array>,
): Promise<Upload> {
  const entries: FileSystemEntry[] = []
  const looseZip = items.length === 1 ? items[0].getAsFile() : null
  for (const item of Array.from(items)) {
    const entry = item.webkitGetAsEntry?.()
    if (entry) entries.push(entry)
  }
  // A single .zip is sent as-is; everything else is packed client-side.
  if (entries.length === 1 && entries[0].isFile && looseZip && /\.zip$/i.test(looseZip.name)) {
    return { blob: looseZip, label: looseZip.name, files: null, roots: [], skipped: [] }
  }
  const collected: Collected[] = []
  const skipped: string[] = []
  for (const entry of entries) {
    // A single dropped folder deploys its contents (like running the CLI
    // inside it), not a wrapper folder.
    if (entries.length === 1 && entry.isDirectory) {
      const reader = (entry as FileSystemDirectoryEntry).createReader()
      for (;;) {
        const batch = await new Promise<FileSystemEntry[]>((resolve, reject) =>
          reader.readEntries(resolve, reject),
        )
        if (batch.length === 0) break
        for (const e of batch) await collectEntry(e, '', collected, skipped)
      }
    } else {
      await collectEntry(entry, '', collected, skipped)
    }
  }
  if (collected.length === 0) {
    throw new Error('Nothing usable was dropped — drop a project folder or a .zip of one.')
  }
  let total = 0
  buffers.clear()
  for (const c of collected) {
    total += c.file.size
    if (total > MAX_UPLOAD) {
      throw new Error('That folder is over 256 MB — deploy it with the CLI instead (donkey deploy).')
    }
    buffers.set(c.path, new Uint8Array(await c.file.arrayBuffer()))
  }
  const label =
    entries.length === 1 ? `${entries[0].name}/ (${collected.length} files)` : `${collected.length} files`
  return {
    blob: null,
    label,
    files: collected,
    roots: candidateRoots(collected.map((c) => c.path)),
    skipped,
  }
}

type Phase =
  | { step: 'pick' }
  | { step: 'ready'; upload: Upload }
  | { step: 'deploying'; upload: Upload }
  | { step: 'done'; result: DeployResult }

export function DeployView({
  space,
  name,
  onDeployed,
}: {
  space: string
  name: string
  onDeployed: (space: string, app: string) => void
}) {
  const [phase, setPhase] = useState<Phase>({ step: 'pick' })
  const [dragOver, setDragOver] = useState(false)
  const [appName, setAppName] = useState(name)
  const [port, setPort] = useState('')
  const [spaceName, setSpaceName] = useState(space)
  const [root, setRoot] = useState('')
  const [lines, setLines] = useState<string[]>([])
  const [buildLog, setBuildLog] = useState('')
  const [error, setError] = useState('')
  const fileInput = useRef<HTMLInputElement>(null)
  const buffers = useRef(new Map<string, Uint8Array>())

  async function accept(makeUpload: () => Promise<Upload>) {
    setError('')
    try {
      const upload = await makeUpload()
      // Nearest app folder wins by default; the picker below appears when
      // there is more than one to choose from.
      setRoot(upload.roots[0]?.dir ?? '')
      setPhase({ step: 'ready', upload })
    } catch (err) {
      setError(errText(err, 'That did not work.'))
    }
  }

  async function deploy(upload: Upload) {
    setPhase({ step: 'deploying', upload })
    setLines([])
    setBuildLog('')
    setError('')
    try {
      const blob = upload.blob ?? zipFrom(upload.files ?? [], root, buffers.current)
      const result = await api.deployUpload(
        blob,
        { space: spaceName.trim(), name: appName.trim(), port: port.trim() },
        (line) => setLines((prev) => [...prev, line]),
        (progress) => setBuildLog((prev) => prev + progress),
      )
      setPhase({ step: 'done', result })
    } catch (err) {
      setError(errText(err, 'The deploy did not work.'))
      setPhase({ step: 'ready', upload })
    }
  }

  // The chosen folder's own donkey.yaml decides; without one the form does.
  const chosen = phase.step === 'ready' ? phase.upload.roots.find((r) => r.dir === root) : undefined
  const hasConfig =
    phase.step === 'ready' ? (phase.upload.files === null ? null : (chosen?.hasConfig ?? false)) : null
  const needsIdentity = phase.step === 'ready' && hasConfig !== true

  return (
    <div className="mx-auto max-w-2xl space-y-5">
      <h1 className="text-lg font-semibold">Deploy</h1>

      {(phase.step === 'pick' || phase.step === 'ready') && (
        <>
          <div
            role="button"
            tabIndex={0}
            aria-label="Drop a project folder or zip here, or press Enter to pick a file"
            onClick={() => fileInput.current?.click()}
            onKeyDown={(e) => {
              if (e.key === 'Enter' || e.key === ' ') fileInput.current?.click()
            }}
            onDragOver={(e) => {
              e.preventDefault()
              setDragOver(true)
            }}
            onDragLeave={() => setDragOver(false)}
            onDrop={(e) => {
              e.preventDefault()
              setDragOver(false)
              const items = e.dataTransfer.items
              if (items.length > 0) accept(() => uploadFromDrop(items, buffers.current))
            }}
            className={`flex cursor-pointer flex-col items-center gap-2 rounded-lg border-2 border-dashed p-10 text-center transition-colors focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-hay-deep ${
              dragOver ? 'border-hay-deep bg-hay-wash' : 'border-line bg-card hover:bg-hay-wash/50'
            }`}
          >
            <UploadCloud className="size-8 text-hay-deep" aria-hidden />
            {phase.step === 'ready' ? (
              <p className="flex items-center gap-2 font-mono text-sm">
                {/\.zip/.test(phase.upload.label) ? (
                  <FileArchive className="size-4 text-stone" aria-hidden />
                ) : (
                  <FolderUp className="size-4 text-stone" aria-hidden />
                )}
                {phase.upload.label}
              </p>
            ) : (
              <>
                <p className="text-sm font-medium">Drop your project folder or a .zip here</p>
                <p className="text-xs text-stone">
                  The same pipeline as <code className="font-mono">donkey deploy</code>: donkey reads
                  the code, builds the image and puts it live.
                </p>
              </>
            )}
            <input
              ref={fileInput}
              type="file"
              accept=".zip"
              className="hidden"
              onChange={(e) => {
                const f = e.target.files?.[0]
                if (f) {
                  accept(async () => ({ blob: f, label: f.name, files: null, roots: [], skipped: [] }))
                }
                e.target.value = ''
              }}
            />
          </div>

          {phase.step === 'ready' && phase.upload.skipped.length > 0 && (
            <p className="text-xs text-stone">
              Left behind (secret-shaped or excluded):{' '}
              {phase.upload.skipped.slice(0, 4).join(', ')}
              {phase.upload.skipped.length > 4 && ` and ${phase.upload.skipped.length - 4} more`}
            </p>
          )}

          {/* Which folder holds the app: automatic for a plain project,
              a choice when the drop turns out to hold several. */}
          {phase.step === 'ready' && phase.upload.roots.length > 1 && (
            <Card className="p-4">
              <SectionTitle>Which app do you want to deploy?</SectionTitle>
              <p className="mt-1 text-xs text-stone">
                This drop holds several. Only the chosen folder is uploaded.
              </p>
              <div className="mt-2 space-y-1">
                {phase.upload.roots.map((r) => (
                  <label key={r.dir} className="flex items-center gap-2 text-sm">
                    <input
                      type="radio"
                      name="approot"
                      checked={root === r.dir}
                      onChange={() => setRoot(r.dir)}
                      className="accent-hay-deep"
                    />
                    <span className="font-mono">{r.dir === '' ? '/ (the whole drop)' : r.dir + '/'}</span>
                    {r.hasConfig && <span className="text-xs text-stone">has donkey.yaml</span>}
                  </label>
                ))}
              </div>
            </Card>
          )}
          {phase.step === 'ready' && phase.upload.roots.length === 1 && root !== '' && (
            <p className="text-xs text-stone">
              Found the app in <span className="font-mono">{root}/</span> — only that folder is uploaded.
            </p>
          )}

          <Card className="space-y-3 p-4">
            <SectionTitle>What is this app?</SectionTitle>
            {hasConfig === true ? (
              <>
                <p className="text-sm text-ink-soft">
                  This folder carries its own <code className="font-mono text-xs">donkey.yaml</code> —
                  its name, port and settings win. The space below still applies.
                </p>
                <label className="block max-w-xs text-sm">
                  <span className="mb-1 block text-xs text-stone">Space (optional)</span>
                  <input
                    value={spaceName}
                    onChange={(e) => setSpaceName(e.target.value)}
                    placeholder="from donkey.yaml"
                    className="w-full rounded-md border border-line bg-surface px-2 py-1.5 font-mono text-sm focus:outline-2 focus:outline-hay-deep"
                  />
                </label>
              </>
            ) : (
              <>
                <p className="text-xs text-stone">
                  Ignored when the upload contains a <code className="font-mono">donkey.yaml</code>.
                </p>
                <div className="grid gap-3 sm:grid-cols-3">
                  <label className="block text-sm">
                    <span className="mb-1 block text-xs text-stone">App name</span>
                    <input
                      value={appName}
                      onChange={(e) => setAppName(e.target.value)}
                      placeholder="my-app"
                      className="w-full rounded-md border border-line bg-surface px-2 py-1.5 font-mono text-sm focus:outline-2 focus:outline-hay-deep"
                    />
                  </label>
                  <label className="block text-sm">
                    <span className="mb-1 block text-xs text-stone">Port it listens on</span>
                    <input
                      value={port}
                      onChange={(e) => setPort(e.target.value)}
                      placeholder="3000"
                      inputMode="numeric"
                      className="w-full rounded-md border border-line bg-surface px-2 py-1.5 font-mono text-sm focus:outline-2 focus:outline-hay-deep"
                    />
                  </label>
                  <label className="block text-sm">
                    <span className="mb-1 block text-xs text-stone">Space (optional)</span>
                    <input
                      value={spaceName}
                      onChange={(e) => setSpaceName(e.target.value)}
                      placeholder="default"
                      className="w-full rounded-md border border-line bg-surface px-2 py-1.5 font-mono text-sm focus:outline-2 focus:outline-hay-deep"
                    />
                  </label>
                </div>
              </>
            )}
          </Card>

          <ErrorNote message={error} />
          <div className="flex justify-end">
            <Button
              onClick={() => phase.step === 'ready' && deploy(phase.upload)}
              disabled={phase.step !== 'ready' || (needsIdentity && (appName.trim() === '' || port.trim() === ''))}
            >
              Deploy
            </Button>
          </div>
        </>
      )}

      {phase.step === 'deploying' && (
        <Card className="space-y-2 p-4">
          <SectionTitle>Deploying…</SectionTitle>
          <ul className="space-y-1 text-sm">
            {lines.map((line, i) => (
              <li key={i}>{line}</li>
            ))}
          </ul>
          {buildLog && (
            <details className="text-xs">
              <summary className="cursor-pointer text-stone">Build output</summary>
              <pre className="mt-1 max-h-64 overflow-auto rounded bg-ink-well p-2 font-mono text-parchment whitespace-pre-wrap">
                {buildLog}
              </pre>
            </details>
          )}
          <ErrorNote message={error} />
        </Card>
      )}

      {phase.step === 'done' && (
        <Card className={`space-y-3 p-5 ${phase.result.ok ? 'border-moss/40' : 'border-brick/40'}`}>
          {phase.result.ok ? (
            <>
              <p className="flex items-center gap-2 text-sm font-medium text-moss">
                <CheckCircle2 className="size-4" aria-hidden />
                {phase.result.app} is running.
              </p>
              {phase.result.url && (
                <a
                  href={phase.result.url}
                  target="_blank"
                  rel="noreferrer"
                  className="flex w-fit items-center gap-1.5 font-mono text-sm text-hay-deep hover:underline"
                >
                  {phase.result.url} <ExternalLink className="size-3.5" aria-hidden />
                </a>
              )}
              <div className="flex gap-2">
                <Button onClick={() => onDeployed(phase.result.space ?? 'default', phase.result.app)}>
                  Open the app page
                </Button>
                <Button kind="quiet" onClick={() => setPhase({ step: 'pick' })}>
                  Deploy another
                </Button>
              </div>
            </>
          ) : (
            <>
              <ErrorNote message={phase.result.error ?? 'The deploy did not work.'} />
              {phase.result.diagnosis && (
                <p className="text-sm">
                  <span className="font-medium">What happened: </span>
                  {phase.result.diagnosis}
                </p>
              )}
              {/* The change itself, not a description of it. A browser deploy
                  has no folder to write back to, so this is something to copy
                  into the project — which only works if it is all here. */}
              {(phase.result.proposals ?? []).map((p, i) => (
                <div key={i} className="rounded-md border border-line bg-card px-3 py-2 text-sm">
                  {p.type === 'set_env' && (
                    <code className="font-mono text-xs">
                      donkey env set {p.key}={p.value}
                    </code>
                  )}
                  {p.type === 'edit_file' && <span className="font-mono text-xs">{p.path}</span>}
                  <p className="mt-0.5 text-xs text-ink-soft">{p.explanation}</p>
                  {p.diff && <DiffView diff={p.diff} />}
                </div>
              ))}
              <Button kind="quiet" onClick={() => setPhase({ step: 'pick' })}>
                Try again
              </Button>
            </>
          )}
        </Card>
      )}
    </div>
  )
}

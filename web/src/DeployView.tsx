// Deploying from the browser: drop a project, donkey reads it, you check what
// it found, it goes live.
//
// The reading is the point. A folder full of source is not something a form
// can ask about — which port, which folders hold data, which services, what
// will break behind a gateway — so nothing is asked before the upload: the
// server reads the code and answers all of it, and this view shows the
// answers as a plan to confirm. That plan is the same one `donkey init`
// prints, and the deploy that follows carries its id, so the app is only
// read once.
import { useRef, useState } from 'react'
import { useQueryClient } from '@tanstack/react-query'
import { CheckCircle2, ExternalLink, FileArchive, FolderUp, Loader2, UploadCloud } from 'lucide-react'
import { zipSync } from 'fflate'
import { api, type DeployResult, type Plan, type PlanEdit } from './api'
import { Button, Card, ErrorNote, errText, SectionTitle } from './ui'
import { DiffView } from './DiffView'
import { PlanReview } from './PlanReview'
import { fleetQuery, useFleet } from './queries'

// The same exclusions the CLI applies while packing — a browser upload
// should not ship what `donkey deploy` would leave behind. The server
// enforces them again for any client; doing it here means the bytes never
// leave the machine in the first place.
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
  // name is what the app should be called if the upload carries no
  // donkey.yaml. Only the browser knows it: the archive it sends is rebased
  // to the app's folder, so the folder's own name is gone by then.
  name: string
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
    return zipUpload(looseZip)
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
    name: entries.length === 1 ? entries[0].name : '',
  }
}

function zipUpload(file: File): Upload {
  return {
    blob: file,
    label: file.name,
    files: null,
    roots: [],
    skipped: [],
    name: file.name.replace(/\.zip$/i, ''),
  }
}

type Phase =
  | { step: 'pick' }
  // Several apps in one drop: which one is a question only the person
  // dropping can answer, and it has to be answered before the upload.
  | { step: 'root'; upload: Upload }
  | { step: 'reading'; upload: Upload }
  | { step: 'review'; upload: Upload; blob: Blob; plan: Plan }
  | { step: 'deploying'; plan: Plan }
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
  const [lines, setLines] = useState<string[]>([])
  const [buildLog, setBuildLog] = useState('')
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)
  const fileInput = useRef<HTMLInputElement>(null)
  const buffers = useRef(new Map<string, Uint8Array>())
  const fleet = useFleet()
  const queryClient = useQueryClient()

  async function accept(makeUpload: () => Promise<Upload>) {
    setError('')
    try {
      const upload = await makeUpload()
      // One app in the drop (the usual case) goes straight to the server —
      // asking "which folder?" when there is only one is noise.
      if (upload.roots.length > 1) {
        setPhase({ step: 'root', upload })
        return
      }
      read(upload, upload.roots[0]?.dir ?? '')
    } catch (err) {
      setError(errText(err, 'That did not work.'))
    }
  }

  // read uploads the project and asks the server what it is. It is the slow
  // step (a model reading real code), and the only one that happens without
  // the user having decided anything — which is the whole idea.
  async function read(upload: Upload, root: string) {
    setError('')
    setPhase({ step: 'reading', upload })
    try {
      const blob = upload.blob ?? zipFrom(upload.files ?? [], root, buffers.current)
      const hint = root ? root.split('/').pop() || upload.name : upload.name
      const plan = await api.plan(blob, { space: space || undefined, name: name || hint })
      setPhase({ step: 'review', upload, blob, plan })
    } catch (err) {
      setError(errText(err, "Donkey couldn't read that project."))
      setPhase({ step: 'pick' })
    }
  }

  // recheck sends the answers back: the server re-decides the config and
  // re-runs the checks that are only true right now (a free name, an existing
  // space, a service already running).
  async function recheck(at: Extract<Phase, { step: 'review' }>, edit: PlanEdit) {
    try {
      const next = await api.updatePlan(at.plan.id, edit)
      setPhase({ ...at, plan: next })
      setError('')
    } catch (err) {
      setError(errText(err, 'That change could not be checked.'))
    }
  }

  async function deploy(at: Extract<Phase, { step: 'review' }>, edit: PlanEdit) {
    setBusy(true)
    setError('')
    try {
      // The answers go up one last time, so what deploys is exactly what the
      // screen showed — including anything typed in the last 400ms.
      const settled = await api.updatePlan(at.plan.id, edit)
      if (!settled.ready) {
        setPhase({ ...at, plan: settled })
        setError('Something still blocks this deploy — see the checks above.')
        return
      }
      setLines([])
      setBuildLog('')
      setPhase({ step: 'deploying', plan: settled })
      const result = await api.deployUpload(
        at.blob,
        { plan: settled.id },
        (line) => setLines((prev) => [...prev, line]),
        (progress) => setBuildLog((prev) => prev + progress),
      )
      queryClient.invalidateQueries({ queryKey: fleetQuery.queryKey })
      setPhase({ step: 'done', result })
    } catch (err) {
      // The plan and the packed project are both still here, so the way back
      // is the review screen — not the drop zone the folder came from.
      setError(errText(err, 'The deploy did not work.'))
      setPhase(at)
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="mx-auto max-w-2xl space-y-5">
      <h1 className="text-lg font-semibold">Deploy</h1>

      {(phase.step === 'pick' || phase.step === 'root') && (
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
            {phase.step === 'root' ? (
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
                  Donkey reads the code first — the port, the folders that hold data, the services it
                  needs — and shows you what it found before anything is built.
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
                if (f) accept(async () => zipUpload(f))
                e.target.value = ''
              }}
            />
          </div>

          {phase.step === 'root' && (
            <Card className="p-4">
              <SectionTitle>Which app do you want to deploy?</SectionTitle>
              <p className="mt-1 text-xs text-stone">
                This drop holds several. Only the chosen folder is uploaded and read.
              </p>
              <div className="mt-3 space-y-1.5">
                {phase.upload.roots.map((r) => (
                  <button
                    key={r.dir}
                    onClick={() => read(phase.upload, r.dir)}
                    className="flex w-full items-center justify-between gap-2 rounded-md border border-line px-3 py-2 text-left text-sm hover:border-line-bright hover:bg-raised"
                  >
                    <span className="font-mono">{r.dir === '' ? '/ (the whole drop)' : r.dir + '/'}</span>
                    {r.hasConfig && <span className="text-xs text-stone">has donkey.yaml</span>}
                  </button>
                ))}
              </div>
            </Card>
          )}

          {phase.step === 'root' && phase.upload.skipped.length > 0 && (
            <p className="text-xs text-stone">
              Left behind (secret-shaped or excluded): {phase.upload.skipped.slice(0, 4).join(', ')}
              {phase.upload.skipped.length > 4 && ` and ${phase.upload.skipped.length - 4} more`}
            </p>
          )}

          <ErrorNote message={error} />
        </>
      )}

      {phase.step === 'reading' && (
        <Card className="space-y-2 p-6">
          <p className="flex items-center gap-2 text-sm">
            <Loader2 className="size-4 animate-spin text-hay" aria-hidden />
            Reading <span className="font-mono">{phase.upload.label}</span>…
          </p>
          <p className="text-xs text-stone">
            Donkey is detecting how to build it and reading the source for the things no detector can
            see — which port it listens on, which folders must survive a restart, which services it
            expects. This takes a moment.
          </p>
        </Card>
      )}

      {phase.step === 'review' && (
        <PlanReview
          plan={phase.plan}
          spaces={fleet.data?.spaces ?? []}
          busy={busy}
          error={error}
          onRecheck={(edit) => recheck(phase, edit)}
          onDeploy={(edit) => deploy(phase, edit)}
          onStartOver={() => {
            setError('')
            setPhase({ step: 'pick' })
          }}
        />
      )}

      {phase.step === 'deploying' && (
        <Card className="space-y-2 p-4">
          <SectionTitle>Deploying {phase.plan.app.name}…</SectionTitle>
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

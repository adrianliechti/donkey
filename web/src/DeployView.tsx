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
import { Loader2, UploadCloud } from 'lucide-react'
import { zipSync } from 'fflate'
import { api, type DeployResult, type DeployUpdate, type ProjectPlan, type PlanEdit } from './api'
import { Button, Card, ErrorNote, errText } from './ui'
import { ProjectReview } from './ProjectReview'
import { ReleaseView } from './ReleaseView'
import { fleetQuery, useFleet } from './queries'

// The same exclusions the CLI applies while packing — a browser upload
// should not ship what `donkey deploy` would leave behind. The server
// enforces them again for any client; doing it here means the bytes never
// leave the machine in the first place.
const SKIP_DIRS = new Set([
  '.git',
  'node_modules',
  '.venv',
  'venv',
  '__pycache__',
  '.idea',
  '.vscode',
])
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

// Upload is what the drop zone produced: the collected files plus what we
// know about them. A ready-made .zip is passed through unopened.
interface Upload {
  label: string
  files: Collected[] | null // null = an opaque .zip, sent as-is
  blob: Blob | null
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
      throw new Error(
        'That folder is over 256 MB — deploy it with the CLI instead (donkey deploy).',
      )
    }
    buffers.set(c.path, new Uint8Array(await c.file.arrayBuffer()))
  }
  const label =
    entries.length === 1
      ? `${entries[0].name}/ (${collected.length} files)`
      : `${collected.length} files`
  return {
    blob: null,
    label,
    files: collected,
    skipped,
    name: entries.length === 1 ? entries[0].name : '',
  }
}

function zipUpload(file: File): Upload {
  return {
    blob: file,
    label: file.name,
    files: null,
    skipped: [],
    name: file.name.replace(/\.zip$/i, ''),
  }
}

type ReviewPhase = { step: 'review'; upload: Upload; blob: Blob; plan: ProjectPlan }
type Phase =
  | { step: 'pick' }
  | { step: 'reading'; upload: Upload }
  | ReviewPhase
  | { step: 'deploying'; plan: ProjectPlan }
  | { step: 'done'; result: DeployResult; plan: ProjectPlan; review: ReviewPhase }

type AppProgress = { lines: string[]; log: string; update: DeployUpdate; result?: DeployResult }

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
  const [progress, setProgress] = useState<Record<string, AppProgress>>({})
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)
  const fileInput = useRef<HTMLInputElement>(null)
  const buffers = useRef(new Map<string, Uint8Array>())
  const fleet = useFleet()
  const queryClient = useQueryClient()

  async function accept(makeUpload: () => Promise<Upload>) {
    setError('')
    setBusy(true)
    try {
      const upload = await makeUpload()
      setPhase({ step: 'reading', upload })
      const blob = upload.blob ?? zipFrom(upload.files ?? [], '', buffers.current)
      const plan = await api.projectPlan(blob, {
        space: space || undefined,
        name: name || upload.name,
      })
      setPhase({ step: 'review', upload, blob, plan })
    } catch (err) {
      setError(errText(err, "Donkey couldn't read that project."))
      setPhase({ step: 'pick' })
    } finally {
      setBusy(false)
    }
  }

  async function recheck(at: ReviewPhase, edits: PlanEdit[]) {
    try {
      const plan = await api.updateProject(at.plan.id, edits)
      setPhase({ ...at, plan })
      setError('')
      return plan
    } catch (err) {
      setError(errText(err, 'The changes could not be checked.'))
      throw err
    }
  }

  async function deploy(at: ReviewPhase, edits: PlanEdit[]) {
    setBusy(true)
    setError('')
    let review = at
    try {
      const settled = await api.updateProject(at.plan.id, edits)
      review = { ...at, plan: settled }
      if (!settled.ready) {
        setPhase(review)
        setError('Something still blocks this deploy — see the checks above.')
        return
      }
      setProgress({})
      setPhase({ step: 'deploying', plan: settled })
      let current = settled.apps[0].app.name
      const update = (apply: (prev: AppProgress) => AppProgress) => {
        const app = current
        setProgress((prev) => ({
          ...prev,
          [app]: apply(prev[app] ?? { lines: [], log: '', update: {} }),
        }))
      }
      const result = await api.deployUpload(
        at.blob,
        { plan: settled.id, project: true },
        (line) => update((prev) => ({ ...prev, lines: [...prev.lines, line] })),
        (text) => update((prev) => ({ ...prev, log: prev.log + text })),
        (event) => {
          if (event.app) current = event.app
          update((prev) => ({
            ...prev,
            update: { ...prev.update, ...event },
            result: event.result ?? prev.result,
          }))
        },
      )
      setPhase({ step: 'done', result, plan: settled, review })
    } catch (err) {
      setError(
        errText(err, 'The deploy did not finish. Check the app pages for the latest status.'),
      )
      setPhase(review)
    } finally {
      queryClient.invalidateQueries({ queryKey: fleetQuery.queryKey })
      queryClient.invalidateQueries({ queryKey: ['status'] })
      queryClient.invalidateQueries({ queryKey: ['builds'] })
      setBusy(false)
    }
  }

  return (
    <div
      className={`mx-auto space-y-5 ${phase.step === 'deploying' || phase.step === 'done' ? 'max-w-6xl' : 'max-w-2xl'}`}
    >
      <h1 className="text-lg font-semibold">Deploy</h1>
      {phase.step === 'pick' && (
        <>
          <div
            role="button"
            tabIndex={0}
            aria-disabled={busy}
            aria-label="Drop a project folder or zip here, or press Enter to pick a file"
            onClick={() => {
              if (!busy) fileInput.current?.click()
            }}
            onKeyDown={(e) => {
              if (!busy && (e.key === 'Enter' || e.key === ' ')) fileInput.current?.click()
            }}
            onDragOver={(e) => {
              e.preventDefault()
              setDragOver(true)
            }}
            onDragLeave={() => setDragOver(false)}
            onDrop={(e) => {
              e.preventDefault()
              setDragOver(false)
              if (!busy && e.dataTransfer.items.length > 0)
                accept(() => uploadFromDrop(e.dataTransfer.items, buffers.current))
            }}
            className={`flex cursor-pointer flex-col items-center gap-2 rounded-lg border-2 border-dashed p-10 text-center transition-colors ${dragOver ? 'border-accent-strong bg-accent-soft' : 'border-line bg-card hover:bg-accent-soft/50'}`}
          >
            <UploadCloud className="size-8 text-accent-strong" aria-hidden />
            <p className="text-sm font-medium">
              {busy ? 'Preparing the upload…' : 'Drop your project folder or a .zip here'}
            </p>
            <p className="text-xs text-muted">
              Donkey finds each app, connects them and prepares their services. Review everything
              before deploying.
            </p>
            <input
              ref={fileInput}
              type="file"
              accept=".zip"
              disabled={busy}
              className="hidden"
              onChange={(e) => {
                const f = e.target.files?.[0]
                if (f) accept(async () => zipUpload(f))
                e.target.value = ''
              }}
            />
          </div>
          <ErrorNote message={error} />
        </>
      )}
      {phase.step === 'reading' && (
        <Card className="space-y-2 p-6">
          <p className="flex items-center gap-2 text-sm">
            <Loader2 className="size-4 animate-spin text-accent" aria-hidden />
            Reading <span className="font-mono">{phase.upload.label}</span>…
          </p>
          <p className="text-xs text-muted">
            Finding runnable apps, shared packages, service dependencies and the URLs that connect
            them. This takes a moment.
          </p>
        </Card>
      )}
      {phase.step === 'review' && (
        <ProjectReview
          key={phase.plan.id}
          plan={phase.plan}
          spaces={fleet.data?.spaces ?? []}
          space={space || undefined}
          busy={busy}
          error={error}
          onCheck={(edits) => recheck(phase, edits)}
          onDeploy={(edits) => deploy(phase, edits)}
          onStartOver={() => {
            setError('')
            setPhase({ step: 'pick' })
          }}
        />
      )}
      {(phase.step === 'deploying' || phase.step === 'done') && (
        <>
          {phase.step === 'done' && <ErrorNote message={phase.result.error || ''} />}
          {phase.plan.apps.map((app) => {
            const status = progress[app.app.name]
            if (!status)
              return (
                <Card key={app.id} className="p-4 text-sm text-muted">
                  {app.app.name} — waiting to deploy
                </Card>
              )
            return (
              <div key={app.id} className="space-y-2">
                <h2 className="text-sm font-semibold">{app.app.name}</h2>
                <ReleaseView
                  plan={app}
                  lines={status.lines}
                  buildLog={status.log}
                  update={status.update}
                  result={status.result}
                  onOpen={() => onDeployed(status.result?.space || app.app.space, app.app.name)}
                />
              </div>
            )
          })}
          {phase.step === 'done' && (
            <div className="flex gap-2">
              <Button
                kind="quiet"
                onClick={() => {
                  setError('')
                  setPhase(phase.review)
                }}
              >
                Back to review
              </Button>
              <Button
                onClick={() => {
                  setError('')
                  setPhase({ step: 'pick' })
                }}
              >
                Deploy another project
              </Button>
            </div>
          )}
        </>
      )}
    </div>
  )
}

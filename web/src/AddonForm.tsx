import { useState, type ReactNode } from 'react'
import { Dialog, DialogBackdrop, DialogPanel, DialogTitle, Radio, RadioGroup } from '@headlessui/react'
import { Archive, ArrowLeft, Database, Server, Sparkles } from 'lucide-react'
import { type AddAddonRequest } from './api'
import { useCatalog } from './queries'
import { Button, ErrorNote, Select, Spinner } from './ui'

const inputStyle =
  'rounded-md border border-line bg-surface px-2 py-1.5 font-mono text-sm focus:outline-2 focus:outline-accent-strong'

// A short ladder of disk sizes is easier to pick right than a free-text
// field is to fill right; the type's own default always leads the list.
const diskSizes = ['1Gi', '2Gi', '5Gi', '10Gi', '20Gi', '50Gi']

// Each catalog entry gets a face; unknown future types fall back to a
// plain server glyph.
const typeIcons: Record<string, typeof Database> = {
  postgres: Database,
  rustfs: Archive,
  openai: Sparkles,
}

const addressExamples: Record<string, string> = {
  postgres: 'postgres://user:password@host:5432/dbname',
  openai: 'https://api.openai.com/v1',
}

function Eyebrow({ children }: { children: ReactNode }) {
  return (
    <span className="font-mono text-[10px] font-semibold uppercase tracking-[0.14em] text-muted">
      {children}
    </span>
  )
}

/**
 * The add-service popup — the space card (create only) and the app page
 * (create and connect in one go) open the same one. Closing is blocked
 * while a request runs: provisioning holds the connection open, and an
 * abandoned dialog would look like nothing happened.
 */
export function AddServiceDialog({
  open,
  title,
  submitLabel,
  busy,
  error,
  note,
  onSubmit,
  onClose,
}: {
  open: boolean
  title: string
  submitLabel: string
  busy: boolean
  error?: string
  note?: string
  onSubmit: (req: AddAddonRequest) => void
  onClose: () => void
}) {
  return (
    <Dialog open={open} onClose={() => !busy && onClose()} className="relative z-50">
      <DialogBackdrop className="fixed inset-0 bg-black/60" />
      <div className="fixed inset-0 flex items-start justify-center p-4 pt-[14vh]">
        <DialogPanel className="w-full max-w-xl rounded-lg border border-line bg-card p-4 shadow-2xl">
          <DialogTitle className="font-mono text-[11px] font-semibold uppercase tracking-[0.14em] text-muted">
            {title}
          </DialogTitle>
          <ErrorNote message={error ?? ''} />
          {note && <p className="mt-2 text-xs text-ink-soft">{note}</p>}
          <div className="mt-3">
            <AddonForm busy={busy} submitLabel={submitLabel} onSubmit={onSubmit} onCancel={onClose} />
          </div>
        </DialogPanel>
      </div>
    </Dialog>
  )
}

/**
 * The catalog, in two steps: first pick the service, then say how it runs.
 * The contract strip on the second step shows the variable names connected
 * apps will receive — the promise that makes managed and external services
 * interchangeable.
 */
export function AddonForm({
  busy,
  submitLabel,
  onSubmit,
  onCancel,
}: {
  busy: boolean
  submitLabel: string
  onSubmit: (req: AddAddonRequest) => void
  onCancel: () => void
}) {
  const { data: catalog, error: catalogError } = useCatalog()
  const types = catalog ?? []
  // An errored fetch counts as loaded — the empty-catalog message says what
  // to do, a spinner would just spin.
  const loaded = catalog !== null || catalogError !== ''
  // '' means the catalog step; a type name means its configuration step.
  const [type, setType] = useState('')
  const [name, setName] = useState('')
  const [storage, setStorage] = useState('')
  const [source, setSource] = useState<'cluster' | 'external'>('cluster')
  const [url, setUrl] = useState('')

  const chosen = types.find((t) => t.type === type)
  const external = source === 'external'
  const needsDisk = !external && Boolean(chosen?.storage)
  const disk = storage || chosen?.storage || ''
  const diskOptions = [...new Set([chosen?.storage ?? '', ...diskSizes])]
    .filter(Boolean)
    .map((s) => ({ value: s, label: s }))

  if (!loaded) return <Spinner label="Opening the catalog…" />
  if (types.length === 0) {
    return (
      <p className="py-6 text-sm text-muted">
        The catalog didn't load — reopen this dialog, or check the server if it keeps happening.
      </p>
    )
  }

  // Step 1 — the catalog. Picking a service moves on; there is nothing
  // else to decide here.
  if (!chosen) {
    return (
      <div className="flex flex-col gap-3">
        <div className="grid gap-2 sm:grid-cols-2">
          {types.map((t) => {
            const Icon = typeIcons[t.type] ?? Server
            return (
              <button
                key={t.type}
                type="button"
                onClick={() => setType(t.type)}
                className="rounded-lg border border-line bg-surface p-3 text-left transition-colors hover:border-accent/70 hover:bg-accent-soft focus-visible:outline-2 focus-visible:outline-accent-strong"
              >
                <div className="flex items-center gap-2">
                  <Icon className="size-4 text-muted" aria-hidden />
                  <span className="font-mono text-sm font-bold">{t.type}</span>
                  <span className="font-mono text-[11px] text-muted">{t.version}</span>
                </div>
                {t.description && (
                  <p className="mt-1.5 text-xs leading-relaxed text-ink-soft">{t.description}</p>
                )}
              </button>
            )
          })}
        </div>
        <div className="flex justify-end">
          <Button kind="quiet" onClick={onCancel}>
            Cancel
          </Button>
        </div>
      </div>
    )
  }

  // Step 2 — how the chosen service runs.
  const ChosenIcon = typeIcons[chosen.type] ?? Server
  const sourceTile =
    'cursor-pointer rounded-lg border border-line bg-surface px-3 py-2 transition-colors hover:border-line-strong focus:outline-2 focus:outline-accent-strong data-checked:border-accent/70 data-checked:bg-accent-soft'

  return (
    <div className="flex flex-col gap-4">
      <div className="flex items-center gap-2">
        <button
          type="button"
          onClick={() => setType('')}
          disabled={busy}
          className="flex items-center gap-1 rounded-md px-1.5 py-1 text-xs text-muted hover:bg-line/50 hover:text-ink focus-visible:outline-2 focus-visible:outline-accent-strong"
        >
          <ArrowLeft className="size-3.5" aria-hidden /> All services
        </button>
        <span className="flex items-center gap-2 rounded-md border border-accent/40 bg-accent-soft px-2 py-1">
          <ChosenIcon className="size-3.5 text-accent" aria-hidden />
          <span className="font-mono text-sm font-bold">{chosen.type}</span>
          <span className="font-mono text-[11px] text-muted">{chosen.version}</span>
        </span>
      </div>

      <div className="flex flex-col gap-1.5">
        <Eyebrow>Where it runs</Eyebrow>
        <RadioGroup
          value={source}
          onChange={setSource}
          aria-label="Where it runs"
          className="grid grid-cols-2 gap-2"
        >
          <Radio value="cluster" className={sourceTile}>
            <span className="text-sm font-medium">In this cluster</span>
            <span className="mt-0.5 block text-xs text-muted">donkey creates and runs it</span>
          </Radio>
          <Radio value="external" className={sourceTile}>
            <span className="text-sm font-medium">Somewhere else</span>
            <span className="mt-0.5 block text-xs text-muted">connect it by its address</span>
          </Radio>
        </RadioGroup>
      </div>

      <div className="flex flex-wrap items-end gap-3">
        <label className="flex flex-col gap-1.5">
          <Eyebrow>Name · optional</Eyebrow>
          <input
            value={name}
            onChange={(e) => setName(e.target.value)}
            placeholder={chosen.type}
            className={`${inputStyle} w-40`}
          />
        </label>
        {needsDisk && (
          <div className="flex flex-col gap-1.5">
            <Eyebrow>Disk</Eyebrow>
            <Select ariaLabel="Disk size" value={disk} onChange={setStorage} options={diskOptions} />
          </div>
        )}
        {external && (
          <label className="flex min-w-0 flex-1 flex-col gap-1.5">
            <Eyebrow>Address</Eyebrow>
            <input
              value={url}
              onChange={(e) => setUrl(e.target.value)}
              placeholder={addressExamples[chosen.type] ?? 'https://service.example.com'}
              className={inputStyle}
            />
          </label>
        )}
      </div>

      <div className="rounded-md border border-line/60 bg-raised/60 px-3 py-2.5">
        <Eyebrow>Your apps will see</Eyebrow>
        {external ? (
          <p className="mt-1 text-xs text-ink-soft">
            The same kind of variables, derived from the address you give — apps cannot tell it from
            a donkey-managed {chosen.type}.
          </p>
        ) : (
          <p className="mt-1 font-mono text-[11px] leading-5 text-ink-soft">
            {(chosen.keys ?? []).join(' · ')}
          </p>
        )}
      </div>

      <div className="flex items-center justify-between gap-2">
        <p className="text-xs text-muted">
          {external
            ? 'Nothing runs in the cluster for this — only the connection is stored.'
            : chosen.version
              ? `${chosen.type} ${chosen.version}${needsDisk ? ` · ${disk} disk` : ''} · shared by the whole space`
              : `${chosen.type} · provided by the platform — nothing runs in your space`}
        </p>
        <div className="flex gap-2">
          <Button kind="quiet" onClick={onCancel} disabled={busy}>
            Cancel
          </Button>
          <Button
            onClick={() =>
              onSubmit({
                type: chosen.type,
                name: name.trim() || undefined,
                storage: needsDisk && storage ? storage : undefined,
                external,
                url: external ? url.trim() : undefined,
              })
            }
            disabled={busy || (external && url.trim() === '')}
          >
            {busy ? 'Setting up…' : submitLabel}
          </Button>
        </div>
      </div>
    </div>
  )
}

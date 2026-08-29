import { useState } from 'react'
import { Link } from '@tanstack/react-router'
import { Dialog, DialogBackdrop, DialogPanel, DialogTitle } from '@headlessui/react'
import { Archive, Database, HardDrive, Network, Plus, Sparkles, Table2, Trash2 } from 'lucide-react'
import { api, type AddAddonRequest, type AppSummary, type SpaceAddonEntry } from './api'
import { AddServiceDialog } from './AddonForm'
import { AiDialog } from './AiDialog'
import { DatabaseDialog } from './DatabaseDialog'
import { useInvalidateServices, useSpaceServices } from './queries'
import { S3Dialog } from './S3Dialog'
import { SpaceTelemetry } from './TrafficPanel'
import { Button, ErrorNote, ExposureBadge, StatusDot, ago, errText, healthLine, healthOf, type Health } from './ui'

/**
 * One section of the paddock per space.
 *
 * A space is the project — it defaults to the repository its apps came from,
 * so a monorepo's frontend and backend land in the same one. Its services
 * (databases & co.) live here too, and its apps sit on the dotted field as
 * cards: the card answers "is it up, how do I reach it, what is attached"
 * without opening anything, and clicking it slides the app's panel over the
 * fleet.
 */
export function SpaceCard({
  space,
  apps,
  onChanged,
}: {
  space: string
  apps: AppSummary[]
  onChanged: () => void
}) {
  const [adding, setAdding] = useState(false)
  const [deletingSpace, setDeletingSpace] = useState(false)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const [typedSpace, setTypedSpace] = useState('')

  const { data: services, error: servicesError } = useSpaceServices(space)
  const invalidate = useInvalidateServices()

  async function add(req: AddAddonRequest) {
    setBusy(true)
    setError('')
    try {
      await api.addSpaceAddon(space, req)
      setAdding(false)
      invalidate(space)
    } catch (err) {
      setError(errText(err, 'Adding did not work.'))
    } finally {
      setBusy(false)
    }
  }

  // The project's own health is the worst of its parts: a frontend that is
  // up is not good news when the backend it calls is down.
  const worst: Health = apps.some((a) => healthOf(a.healthy, a.ready) === 'bad')
    ? 'bad'
    : apps.some((a) => healthOf(a.healthy, a.ready) === 'unknown')
      ? 'unknown'
      : 'ok'

  return (
    <section className="rounded-xl border border-line/70 bg-canvas/60 p-4">
      <div className="mb-3 flex items-center justify-between gap-3">
        <div className="flex items-center gap-2.5">
          <StatusDot health={worst} />
          <h2 className="font-mono text-base font-bold tracking-tight">{space}</h2>
        </div>
        <div className="flex items-center gap-0.5">
          <SpaceTelemetry space={space} />
          <span className="mx-1 h-4 w-px bg-line" aria-hidden />
          <button
            type="button"
            title={`Add a service to ${space} (database & co.)`}
            aria-label={`Add a service to ${space}`}
            onClick={() => setAdding(true)}
            className="rounded-md p-1.5 text-muted hover:bg-line/60 hover:text-ink focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-accent-strong"
          >
            <Plus className="size-4" aria-hidden />
          </button>
          <button
            type="button"
            title={`Remove the space ${space} and everything in it`}
            aria-label={`Remove the space ${space} and everything in it`}
            onClick={() => {
              setError('')
              setTypedSpace('')
              setDeletingSpace(true)
            }}
            className="rounded-md p-1.5 text-muted hover:bg-danger/10 hover:text-danger focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-accent-strong"
          >
            <Trash2 className="size-4" aria-hidden />
          </button>
        </div>
      </div>

      <SpaceServices space={space} services={services} onChanged={() => invalidate(space)} />

      {apps.length === 0 && (
        <p className="text-sm text-muted">
          No apps yet — deploy into this space with{' '}
          <code className="rounded bg-surface px-1.5 py-0.5 font-mono text-xs">space: {space}</code> in
          donkey.yaml, or the Deploy button above.
        </p>
      )}

      <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-3">
        {apps.map((app) => (
          <AppCard key={app.name} app={app} />
        ))}
      </div>

      {deletingSpace && (
        <Dialog
          open
          onClose={() => {
            if (!busy) {
              setDeletingSpace(false)
              setTypedSpace('')
            }
          }}
          className="relative z-50"
        >
          <DialogBackdrop className="fixed inset-0 bg-black/60" />
          <div className="fixed inset-0 flex items-start justify-center p-4 pt-[18vh]">
            <DialogPanel className="w-full max-w-md rounded-lg border border-line bg-card p-4 shadow-2xl">
              <DialogTitle className="font-mono text-[11px] font-semibold uppercase tracking-[0.14em] text-muted">
                Remove the space {space}
              </DialogTitle>
              <ErrorNote message={error} />
              <div className="mt-3 space-y-2 text-sm text-ink-soft">
                <p>This permanently removes the space and every Donkey-managed resource in it.</p>
                {apps.length > 0 ? (
                  <p className="text-danger">
                    Its {apps.length === 1 ? 'app' : 'apps'}{' '}
                    <span className="font-mono">{apps.map((app) => app.name).join(', ')}</span> will be deleted.
                  </p>
                ) : (
                  <p className="text-muted">It holds no apps.</p>
                )}
                {services === null ? (
                  <p className={servicesError ? 'text-danger' : 'text-muted'}>
                    {servicesError
                      ? 'The managed-service inventory could not be loaded. The server will still remove every owned service.'
                      : 'Loading the managed-service inventory…'}
                  </p>
                ) : services.length > 0 ? (
                  <p className="text-danger">
                    Its {services.length === 1 ? 'service' : 'services'}{' '}
                    <span className="font-mono">{services.map((s) => s.name).join(', ')}</span>{' '}
                    and all their data will be deleted with it.
                  </p>
                ) : (
                  <p className="text-muted">It holds no managed services.</p>
                )}
                <p className="text-danger">
                  All app disks and managed-service data will be deleted. This action has no keep-data option.
                </p>
              </div>
              <label className="mt-3 block text-sm text-ink-soft">
                Type <span className="font-mono font-bold">{space}</span> to confirm
                <input
                  autoFocus
                  value={typedSpace}
                  onChange={(event) => setTypedSpace(event.target.value)}
                  disabled={busy}
                  className="mt-1 w-full rounded-md border border-line bg-canvas px-2 py-1 font-mono text-sm"
                />
              </label>
              <div className="mt-4 flex justify-end gap-2">
                <Button
                  kind="quiet"
                  onClick={() => {
                    setDeletingSpace(false)
                    setTypedSpace('')
                  }}
                  disabled={busy}
                >
                  Cancel
                </Button>
                <Button
                  kind="danger"
                  disabled={busy || (services === null && !servicesError) || typedSpace.trim() !== space}
                  onClick={async () => {
                    setBusy(true)
                    setError('')
                    try {
                      await api.deleteSpace(space)
                      setDeletingSpace(false)
                      setTypedSpace('')
                      onChanged()
                    } catch (err) {
                      setError(errText(err, 'Removing did not work.'))
                    } finally {
                      setBusy(false)
                    }
                  }}
                >
                  {busy ? 'Removing…' : `Remove ${space}`}
                </Button>
              </div>
            </DialogPanel>
          </div>
        </Dialog>
      )}

      <AddServiceDialog
        open={adding}
        title={`Add a service to ${space}`}
        submitLabel="Add to the space"
        busy={busy}
        error={error}
        note={busy ? 'Setting up — the first time can take a minute while the cluster downloads the image.' : ''}
        onSubmit={add}
        onClose={() => {
          setAdding(false)
          setError('')
        }}
      />
    </section>
  )
}

/**
 * The space's services as a chip row. Removing one has its own data-loss
 * confirmation; removing the whole space above is the other explicit
 * cascade. Apps merely disconnect services in their own panel.
 */
function SpaceServices({
  space,
  services,
  onChanged,
}: {
  space: string
  services: SpaceAddonEntry[] | null
  onChanged: () => void
}) {
  const [removing, setRemoving] = useState<SpaceAddonEntry | null>(null)
  const [browsing, setBrowsing] = useState<SpaceAddonEntry | null>(null)
  const [storeBrowsing, setStoreBrowsing] = useState<SpaceAddonEntry | null>(null)
  const [chatting, setChatting] = useState<SpaceAddonEntry | null>(null)

  if (!services || services.length === 0) return null

  return (
    <div className="mb-3">
      <div className="flex flex-wrap items-center gap-2">
        {services.map((svc) => (
          <span
            key={svc.name}
            className="inline-flex items-center gap-1.5 rounded border border-line bg-raised px-2 py-1 font-mono text-xs text-ink-soft"
            title={
              svc.attachedTo.length > 0
                ? `Used by ${svc.attachedTo.join(', ')}`
                : 'Not connected to any app yet'
            }
          >
            <Database className="size-3" aria-hidden />
            {svc.name}
            {svc.keys.includes('OPENAI_BASE_URL') && (
              <button
                type="button"
                title={`Try the ${svc.name} service in a chat`}
                aria-label={`Try the ${svc.name} service in a chat`}
                onClick={() => setChatting(svc)}
                className="rounded p-0.5 text-muted hover:bg-line/50 hover:text-ink"
              >
                <Sparkles className="size-3" aria-hidden />
              </button>
            )}
            {svc.keys.includes('AWS_ENDPOINT_URL_S3') && (
              <button
                type="button"
                title={`Browse the ${svc.name} buckets`}
                aria-label={`Browse the ${svc.name} buckets`}
                onClick={() => setStoreBrowsing(svc)}
                className="rounded p-0.5 text-muted hover:bg-line/50 hover:text-ink"
              >
                <Archive className="size-3" aria-hidden />
              </button>
            )}
            {svc.keys.includes('DATABASE_URL') && (
              <button
                type="button"
                title={`Browse the ${svc.name} database`}
                aria-label={`Browse the ${svc.name} database`}
                onClick={() => setBrowsing(svc)}
                className="rounded p-0.5 text-muted hover:bg-line/50 hover:text-ink"
              >
                <Table2 className="size-3" aria-hidden />
              </button>
            )}
            <button
              type="button"
              title={`Remove ${svc.name} from ${space}`}
              aria-label={`Remove ${svc.name} from ${space}`}
              onClick={() => setRemoving(svc)}
              className="rounded p-0.5 text-muted hover:bg-danger/10 hover:text-danger"
            >
              <Trash2 className="size-3" aria-hidden />
            </button>
          </span>
        ))}
      </div>
      {browsing && (
        <DatabaseDialog space={space} service={browsing.name} onClose={() => setBrowsing(null)} />
      )}
      {storeBrowsing && (
        <S3Dialog space={space} service={storeBrowsing.name} onClose={() => setStoreBrowsing(null)} />
      )}
      {chatting && <AiDialog space={space} service={chatting.name} onClose={() => setChatting(null)} />}
      {removing && (
        <RemoveServiceDialog
          space={space}
          service={removing}
          onClose={() => setRemoving(null)}
          onRemoved={() => {
            setRemoving(null)
            onChanged()
          }}
        />
      )}
    </div>
  )
}

/**
 * The one destructive act in the addon world gets a proper stop: it says
 * who loses the service, and deleting the data is the default — keeping a
 * disk around is the deliberate choice, not the accident.
 */
function RemoveServiceDialog({
  space,
  service,
  onClose,
  onRemoved,
}: {
  space: string
  service: SpaceAddonEntry
  onClose: () => void
  onRemoved: () => void
}) {
  const [keepData, setKeepData] = useState(false)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')

  async function remove() {
    setBusy(true)
    setError('')
    try {
      await api.removeSpaceAddon(space, service.name, keepData)
      onRemoved()
    } catch (err) {
      setError(errText(err, 'Removing did not work.'))
    } finally {
      setBusy(false)
    }
  }

  return (
    <Dialog open onClose={() => !busy && onClose()} className="relative z-50">
      <DialogBackdrop className="fixed inset-0 bg-black/60" />
      <div className="fixed inset-0 flex items-start justify-center p-4 pt-[18vh]">
        <DialogPanel className="w-full max-w-md rounded-lg border border-line bg-card p-4 shadow-2xl">
          <DialogTitle className="font-mono text-[11px] font-semibold uppercase tracking-[0.14em] text-muted">
            Remove {service.name} from {space}
          </DialogTitle>
          <ErrorNote message={error} />
          <div className="mt-3 space-y-2 text-sm text-ink-soft">
            {service.attachedTo.length > 0 ? (
              <p>
                <span className="font-mono">{service.attachedTo.join(', ')}</span>{' '}
                {service.attachedTo.length === 1 ? 'is' : 'are'} connected — {service.attachedTo.length === 1 ? 'it' : 'they'} will restart
                without its variables.
              </p>
            ) : (
              <p>No app is connected to it.</p>
            )}
            {keepData ? (
              <p className="text-muted">
                Its disk stays — adding a service with the same name later picks the data back up.
              </p>
            ) : (
              <p className="text-danger">Its disk and everything on it will be deleted.</p>
            )}
          </div>
          <label className="mt-3 flex items-center gap-1.5 text-xs text-ink-soft">
            <input
              type="checkbox"
              checked={keepData}
              onChange={(e) => setKeepData(e.target.checked)}
              className="size-3.5 accent-accent-strong"
            />
            Keep its data
          </label>
          <div className="mt-4 flex justify-end gap-2">
            <Button kind="quiet" onClick={onClose} disabled={busy}>
              Cancel
            </Button>
            <Button kind="danger" onClick={remove} disabled={busy}>
              {busy
                ? 'Removing…'
                : keepData
                  ? `Remove ${service.name}, keep data`
                  : `Remove ${service.name} and its data`}
            </Button>
          </div>
        </DialogPanel>
      </div>
    </Dialog>
  )
}

function AppCard({ app }: { app: AppSummary }) {
  const h = healthOf(app.healthy, app.ready)
  const edge =
    h === 'bad' ? 'border-danger/50 hover:border-danger' : 'border-line hover:border-line-strong'

  return (
    <div className={`rounded-lg border bg-card transition-colors ${edge}`}>
      {/* A real router Link: modifier clicks, middle clicks and copy-link
          behave like the web, with none of the hand-rolled handling. */}
      <Link
        to="/app/$space/$name/{-$tab}"
        params={{ space: app.space, name: app.name, tab: undefined }}
        className="block p-3.5 focus-visible:outline-2 focus-visible:-outline-offset-2 focus-visible:outline-accent-strong"
      >
        <div className="flex items-center justify-between gap-2">
          <span className="flex min-w-0 items-center gap-2">
            <StatusDot health={h} />
            <span className="truncate font-mono text-sm font-bold">{app.name}</span>
          </span>
          <ExposureBadge exposure={app.exposure} />
        </div>

        <div className="mt-2 space-y-1 text-xs">
          {app.url ? (
            <div className="truncate font-mono text-muted">{app.url.replace(/^https?:\/\//, '')}</div>
          ) : (
            <div
              className="flex items-center gap-1 truncate font-mono text-muted"
              title={`No address of its own — its siblings in ${app.space} reach it as http://${app.name}`}
            >
              <Network className="size-3 shrink-0" aria-hidden /> http://{app.name}
            </div>
          )}
          <div className="flex flex-wrap items-center gap-x-2 gap-y-0.5 text-muted">
            <span className={h === 'bad' ? 'text-danger' : ''}>{healthLine[h](app.ready)}</span>
            {app.startedAt && <span>· up {ago(app.startedAt)}</span>}
            {(app.restarts ?? 0) > 0 && (
              <span className="text-danger" title={`${app.restarts} restarts`}>
                · {app.restarts}↻
              </span>
            )}
          </div>
        </div>

        {((app.addons ?? []).length > 0 || app.hasStorage) && (
          <div className="mt-2.5 flex flex-wrap items-center gap-1.5 border-t border-line/70 pt-2">
            {(app.addons ?? []).map((addon) => (
              <span
                key={addon}
                className="inline-flex items-center gap-1 rounded bg-raised px-1.5 py-0.5 font-mono text-[10px] text-ink-soft"
                title={`Connected service: ${addon}`}
              >
                <Database className="size-3" aria-hidden /> {addon}
              </span>
            ))}
            {app.hasStorage && (
              <span
                className="inline-flex items-center gap-1 rounded bg-raised px-1.5 py-0.5 font-mono text-[10px] text-ink-soft"
                title="Owns a disk — browse it in the Files tab"
              >
                <HardDrive className="size-3" aria-hidden /> disk
              </span>
            )}
          </div>
        )}
      </Link>

    </div>
  )
}

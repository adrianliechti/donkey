import { lazy, Suspense, useState } from 'react'
import { Link, useNavigate } from '@tanstack/react-router'
import {
  Dialog,
  DialogBackdrop,
  DialogPanel,
  DialogTitle,
  Menu,
  MenuButton,
  MenuItem,
  MenuItems,
} from '@headlessui/react'
import {
  Database,
  EllipsisVertical,
  HardDrive,
  Network,
  Plus,
  Rocket,
  Settings,
  Trash2,
} from 'lucide-react'
import { api, type AddAddonRequest, type AppSummary, type SpaceAddonEntry } from './api'
import { AddServiceDialog } from './AddonForm'
import { summarizeParameters } from './parameters'
import { useCatalog, useInvalidateServices, useSpaceServices } from './queries'
import { serviceUI } from './serviceProtocol'
import { SpaceTelemetry } from './TrafficPanel'
import { SpaceSettings } from './SpaceSettings'
import { Button, ErrorNote, ExposureBadge, Spinner, ago, errText, healthLine, healthOf } from './ui'

const DatabaseDialog = lazy(() => import('./DatabaseDialog').then((m) => ({ default: m.DatabaseDialog })))
const RedisDialog = lazy(() => import('./RedisDialog').then((m) => ({ default: m.RedisDialog })))
const S3Dialog = lazy(() => import('./S3Dialog').then((m) => ({ default: m.S3Dialog })))
const AiDialog = lazy(() => import('./AiDialog').then((m) => ({ default: m.AiDialog })))

/**
 * One section of the paddock per space.
 *
 * A space groups deployed apps and services. A project's frontend and backend
 * land in the same space. Its services
 * (databases & co.) live here too, and its apps sit on the dotted field as
 * cards: the card answers "is it up, how do I reach it, what is attached"
 * without opening anything, and clicking it slides the app's panel over the
 * fleet.
 */
export function SpaceCard({
  space,
  apps,
  allApps,
  role,
  onChanged,
}: {
  space: string
  /** The apps to show in the grid — may be filtered (e.g. the fleet's
   *  "Only problems" view). */
  apps: AppSummary[]
  /** Every app in the space, filtered or not. The delete confirmation must
   *  count from this, or it under-reports what a delete destroys. Defaults to
   *  `apps` when the caller isn't filtering. */
  allApps?: AppSummary[]
  role: 'admin' | 'developer'
  onChanged: () => void
}) {
  const everyApp = allApps ?? apps
  // null = closed; a type name = the add-service dialog open on that service.
  const [addingType, setAddingType] = useState<string | null>(null)
  const [deletingSpace, setDeletingSpace] = useState(false)
  const [editingSettings, setEditingSettings] = useState(false)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const [typedSpace, setTypedSpace] = useState('')

  const { data: services, error: servicesError } = useSpaceServices(space)
  const { data: catalog } = useCatalog()
  const invalidate = useInvalidateServices()
  const addingTitle = catalog?.find((s) => s.type === addingType)?.title || addingType

  async function add(req: AddAddonRequest) {
    setBusy(true)
    setError('')
    try {
      await api.addSpaceAddon(space, req)
      setAddingType(null)
      invalidate(space)
    } catch (err) {
      setError(errText(err, 'Adding did not work.'))
    } finally {
      setBusy(false)
    }
  }

  return (
    <section className="py-6">
      <div className="mb-3 flex items-center justify-between gap-3">
        <div className="flex items-center gap-2.5">
          <h2 className="font-mono text-base font-bold tracking-tight">{space}</h2>
        </div>
        <div className="flex items-center gap-0.5">
          <SpaceTelemetry space={space} />
          <span className="mx-1 h-4 w-px bg-line" aria-hidden />
          <AddMenu space={space} onAddService={(type) => setAddingType(type)} />
          {role === 'admin' && <SpaceMenu
            space={space}
            onSettings={() => setEditingSettings(true)}
            onDelete={() => {
              setError('')
              setTypedSpace('')
              setDeletingSpace(true)
            }}
          />}
        </div>
      </div>

      <SpaceServices space={space} services={services} onChanged={() => invalidate(space)} />
      {editingSettings && <SpaceSettings space={space} onClose={() => setEditingSettings(false)} onSaved={onChanged} />}

      {apps.length === 0 && (
        <p className="text-sm text-muted">
          No apps yet — add one with{' '}
          <span className="font-medium text-ink-soft">+ → Application</span> above, or deploy with{' '}
          <code className="rounded bg-surface px-1.5 py-0.5 font-mono text-xs">space: {space}</code> in
          donkey.yaml.
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
          <DialogBackdrop
            transition
            className="fixed inset-0 bg-black/60 transition-opacity duration-(--duration-base) ease-emphasized data-closed:opacity-0 data-leave:duration-(--duration-fast) data-leave:ease-exit"
          />
          <div className="fixed inset-0 flex items-start justify-center p-4 pt-[18vh]">
            <DialogPanel
              transition
              className="w-full max-w-md rounded-lg border border-line bg-card p-4 shadow-2xl transition duration-(--duration-base) ease-emphasized data-closed:opacity-0 data-closed:scale-95 data-leave:duration-(--duration-fast) data-leave:ease-exit"
            >
              <DialogTitle className="font-mono text-[11px] font-semibold uppercase tracking-[0.14em] text-muted">
                Remove the space {space}
              </DialogTitle>
              <ErrorNote message={error} />
              <div className="mt-3 space-y-2 text-sm text-ink-soft">
                <p>This permanently removes the space and every Donkey-managed resource in it.</p>
                {everyApp.length > 0 ? (
                  <p className="text-danger">
                    Its {everyApp.length === 1 ? 'app' : 'apps'}{' '}
                    <span className="font-mono">{everyApp.map((app) => app.name).join(', ')}</span> will be deleted.
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
        open={addingType !== null}
        title={addingType ? `Add ${addingTitle} to ${space}` : ''}
        submitLabel="Add"
        busy={busy}
        error={error}
        initialType={addingType ?? undefined}
        note={busy ? 'Setting up — the first time can take a minute while the cluster downloads the image.' : ''}
        onSubmit={add}
        onClose={() => {
          setAddingType(null)
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

  if (!services || services.length === 0) return null

  const consoleKind = serviceUI(browsing?.protocol).console
  const consoleProps = {
    space, service: browsing?.name ?? '',
    onClose: () => setBrowsing(null),
    onDelete: () => { setRemoving(browsing); setBrowsing(null) },
  }
  const chip =
    'inline-flex items-center gap-1.5 rounded border border-line bg-raised px-2 py-1 font-mono text-xs text-ink-soft'

  return (
    <div className="mb-3">
      <div className="flex flex-wrap items-center gap-2">
        {services.map((svc) => {
          const view = serviceUI(svc.protocol)
          const settings = summarizeParameters(svc.parameters)
          const used =
            (svc.attachedTo.length > 0
              ? `Used by ${svc.attachedTo.join(', ')}`
              : 'Not connected to any app yet') + (settings ? ` · ${settings}` : '')
          // A plugin instance that is still being set up, failed, or was
          // stopped with its data kept has nothing to browse: say what it is
          // and offer the way out.
          const unready = svc.state && svc.state !== 'ready'
          if (!view.console || unready) {
            const note = unready ? `${svc.state}${svc.message ? `: ${svc.message}` : ''}` : ''
            return (
              <span key={svc.name} className={`${chip} ${svc.state === 'failed' ? 'border-danger/50' : ''}`} title={note ? `${note} — ${used}` : used}>
                <view.Icon className="size-3" aria-hidden />
                {svc.name}
                {unready && (
                  <span className={`rounded px-1 text-[10px] uppercase tracking-wide ${svc.state === 'failed' ? 'bg-danger/10 text-danger' : 'bg-line/60 text-muted'}`}>
                    {svc.state}
                  </span>
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
            )
          }
          return (
            <button
              key={svc.name}
              type="button"
              onClick={() => setBrowsing(svc)}
              title={`Open ${svc.name} ${view.noun} — ${used}`}
              className={`${chip} transition-colors duration-(--duration-fast) hover:border-accent/70 hover:bg-accent-soft hover:text-ink focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-accent-strong`}
            >
              <view.Icon className="size-3" aria-hidden />
              {svc.name}
            </button>
          )
        })}
      </div>
      <Suspense fallback={<Spinner label="Opening service…" />}>
        {browsing && consoleKind === 'database' && <DatabaseDialog {...consoleProps} protocol={browsing.protocol} />}
        {browsing && consoleKind === 's3' && <S3Dialog {...consoleProps} />}
        {browsing && consoleKind === 'redis' && <RedisDialog {...consoleProps} />}
        {browsing && consoleKind === 'openai' && <AiDialog {...consoleProps} />}
      </Suspense>
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
  // A plugin-provided service whose plugin is gone or failing cannot be
  // removed properly; once the server says so, offer to drop donkey's record
  // and leave the backing service behind.
  const [orphan, setOrphan] = useState(false)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const canOrphan = error.includes('--orphan')

  async function remove() {
    setBusy(true)
    setError('')
    try {
      await api.removeSpaceAddon(space, service.name, keepData, orphan)
      onRemoved()
    } catch (err) {
      setError(errText(err, 'Removing did not work.'))
    } finally {
      setBusy(false)
    }
  }

  return (
    <Dialog open onClose={() => !busy && onClose()} className="relative z-50">
      <DialogBackdrop
            transition
            className="fixed inset-0 bg-black/60 transition-opacity duration-(--duration-base) ease-emphasized data-closed:opacity-0 data-leave:duration-(--duration-fast) data-leave:ease-exit"
          />
      <div className="fixed inset-0 flex items-start justify-center p-4 pt-[18vh]">
        <DialogPanel
              transition
              className="w-full max-w-md rounded-lg border border-line bg-card p-4 shadow-2xl transition duration-(--duration-base) ease-emphasized data-closed:opacity-0 data-closed:scale-95 data-leave:duration-(--duration-fast) data-leave:ease-exit"
            >
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
            {service.state && service.state !== 'ready' && (
              <p className="text-muted">
                It is {service.state}{service.message ? `: ${service.message}` : ''}.
              </p>
            )}
            {orphan ? (
              <p className="text-muted">The backing service and its data stay with the operator. Donkey forgets this instance.</p>
            ) : keepData ? (
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
              disabled={orphan || busy}
              onChange={(e) => setKeepData(e.target.checked)}
              className="size-3.5 accent-accent-strong"
            />
            Keep its data
          </label>
          {canOrphan && (
            <label className="mt-2 flex items-center gap-1.5 text-xs text-ink-soft">
              <input
                type="checkbox"
                checked={orphan}
                disabled={busy}
                onChange={(e) => {
                  setOrphan(e.target.checked)
                  if (e.target.checked) setKeepData(false)
                }}
                className="size-3.5 accent-accent-strong"
              />
              Its plugin is not answering — drop donkey's record anyway and leave the backing service to the operator
            </label>
          )}
          <div className="mt-4 flex justify-end gap-2">
            <Button kind="quiet" onClick={onClose} disabled={busy}>
              Cancel
            </Button>
            <Button kind="danger" onClick={remove} disabled={busy}>
              {busy
                ? 'Removing…'
                : orphan
                  ? `Forget ${service.name}`
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

// The space's "+" — everything you can add here, named. An app goes to the
// deploy flow; each service in the catalog is its own entry, so a two-click
// "pick from a grid" popup is gone: click the one you want and configure it.
function AddMenu({ space, onAddService }: { space: string; onAddService: (type: string) => void }) {
  const navigate = useNavigate()
  const { data: catalog } = useCatalog()
  const services = catalog ?? []
  return (
    <Menu>
      <MenuButton
        title={`Add to ${space}`}
        aria-label={`Add to ${space}`}
        className="flex items-center rounded-md p-1.5 text-muted hover:bg-line/60 hover:text-ink focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-accent-strong data-open:bg-line/60 data-open:text-ink"
      >
        <Plus className="size-4" aria-hidden />
      </MenuButton>
      <MenuItems
        anchor="bottom end"
        className="z-50 mt-1 w-72 overflow-hidden rounded-md border border-line bg-card py-1 shadow-lg focus:outline-none"
      >
        <MenuItem>
          <button
            type="button"
            onClick={() => navigate({ to: '/deploy/$space', params: { space } })}
            className="flex w-full items-start gap-2.5 px-3 py-2 text-left data-focus:bg-accent-soft"
          >
            <Rocket className="mt-0.5 size-4 shrink-0 text-muted" aria-hidden />
            <span className="min-w-0">
              <span className="block text-sm font-medium text-ink">Application</span>
              <span className="block text-xs text-muted">Deploy a project folder into {space}.</span>
            </span>
          </button>
        </MenuItem>
        {services.length > 0 && <div className="my-1 h-px bg-line" aria-hidden />}
        {services.map((svc) => {
          const { Icon } = serviceUI(svc.icon || svc.protocol)
          return (
            <MenuItem key={svc.type}>
              <button
                type="button"
                onClick={() => onAddService(svc.type)}
                className="flex w-full items-start gap-2.5 px-3 py-2 text-left data-focus:bg-accent-soft"
              >
                <Icon className="mt-0.5 size-4 shrink-0 text-muted" aria-hidden />
                <span className="min-w-0">
                  <span className="block text-sm font-medium text-ink">{svc.title || svc.type}</span>
                  {svc.description && (
                    <span className="block text-xs text-muted">{svc.description}</span>
                  )}
                </span>
              </button>
            </MenuItem>
          )
        })}
      </MenuItems>
    </Menu>
  )
}

// The space's overflow menu. Deleting a space is the one destructive act here,
// so it sits behind a kebab rather than a bare trash icon a mis-click reaches.
function SpaceMenu({ space, onDelete, onSettings }: { space: string; onDelete: () => void; onSettings: () => void }) {
  return (
    <Menu>
      <MenuButton
        title={`More actions for ${space}`}
        aria-label={`More actions for ${space}`}
        className="flex items-center rounded-md p-1.5 text-muted hover:bg-line/60 hover:text-ink focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-accent-strong data-open:bg-line/60 data-open:text-ink"
      >
        <EllipsisVertical className="size-4" aria-hidden />
      </MenuButton>
      <MenuItems
        anchor="bottom end"
        className="z-50 mt-1 w-48 overflow-hidden rounded-md border border-line bg-card py-1 shadow-lg focus:outline-none"
      >
		<MenuItem>
		  <button type="button" onClick={onSettings}
		    className="flex w-full items-center gap-2 px-3 py-2 text-left text-sm text-ink-soft data-focus:bg-accent-soft">
		    <Settings className="size-4" aria-hidden /> Space settings
		  </button>
		</MenuItem>
        <MenuItem>
          <button
            type="button"
            onClick={onDelete}
            className="flex w-full items-center gap-2 px-3 py-2 text-left text-sm text-danger data-focus:bg-danger/10"
          >
            <Trash2 className="size-4" aria-hidden /> Delete space
          </button>
        </MenuItem>
      </MenuItems>
    </Menu>
  )
}

function AppCard({ app }: { app: AppSummary }) {
  const h = healthOf(app.healthy, app.ready)
  // The card's own border carries its health — no separate dot to scatter the
  // eye. Healthy stays calm and neutral; a problem tints the whole edge.
  const edge =
    h === 'bad'
      ? 'border-danger/60 hover:border-danger'
      : h === 'unknown'
        ? 'border-muted/40 hover:border-muted'
        : 'border-line hover:border-line-strong'

  return (
    <div
      className={`rounded-lg border bg-card transition-[color,background-color,border-color,box-shadow,translate] duration-(--duration-fast) ease-emphasized hover:shadow-sm motion-safe:hover:-translate-y-px ${edge}`}
    >
      {/* A real router Link: modifier clicks, middle clicks and copy-link
          behave like the web, with none of the hand-rolled handling. */}
      <Link
        to="/app/$space/$name/{-$tab}"
        params={{ space: app.space || 'default', name: app.name, tab: undefined }}
        className="block p-3.5 focus-visible:outline-2 focus-visible:-outline-offset-2 focus-visible:outline-accent-strong"
      >
        <div className="flex items-center justify-between gap-2">
          <span className="truncate font-mono text-sm font-bold">{app.name}</span>
		  <ExposureBadge exposure={app.exposure} mode={app.mode} />
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

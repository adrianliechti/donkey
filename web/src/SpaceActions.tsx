import { useState } from 'react'
import { useNavigate, useParams } from '@tanstack/react-router'
import { useQueryClient } from '@tanstack/react-query'
import { Menu, MenuButton, MenuItem, MenuItems } from '@headlessui/react'
import { Activity, EllipsisVertical, Info, PlusSquare, Rocket, Settings, Trash2, TriangleAlert } from 'lucide-react'
import type { AppSummary, SpaceAddonEntry } from './api'
import { AddServiceDialog } from './AddonForm'
import { useDeploy } from './deploy'
import { Modal, ModalBody, ModalFooter, ModalHeader, ModalNotice } from './dialog'
import {
  fleetQuery,
  mutationError,
  useAddService,
  useCatalog,
  useDeleteSpace,
  useFleet,
  useRemoveService,
  useSpaceServices,
} from './queries'
import { SpaceSettings } from './SpaceSettings'
import { SpaceTrafficDrawer } from './TrafficPanel'
import { Button, Callout, Field, Input, ToggleRow } from './ui'

const menuTrigger =
  'flex size-7 shrink-0 items-center justify-center rounded-md text-muted hover:bg-line/60 hover:text-ink focus-visible:outline-2 focus-visible:outline-accent-strong data-open:bg-line/60 data-open:text-ink'
const menuPanel = 'z-50 mt-1 overflow-hidden rounded-lg border border-line bg-card py-1 shadow-xl focus:outline-none'
const menuItem =
  'flex min-h-9 w-full items-center gap-2.5 px-3 text-left text-sm text-ink-soft data-focus:bg-accent-soft data-focus:text-ink'

type Dialog = 'add' | 'settings' | 'delete' | 'traffic'

/**
 * Everything that can be done to a space, behind the ⋯ beside its name in
 * the breadcrumb: put something into it, look at its traffic, change who
 * may use it, or delete it. Deleting is the one destructive act, so
 * it sits last and wants the name typed back.
 */
export function SpaceActions({ space }: { space: string }) {
  const { data } = useFleet()
  const openDeploy = useDeploy()
  const [dialog, setDialog] = useState<Dialog | null>(null)
  const role = data?.roles[space] ?? 'viewer'
  const apps = (data?.apps ?? []).filter((app) => (app.space || 'default') === space)
  const close = () => setDialog(null)

  return (
    <>
      <Menu>
        <MenuButton title={`Actions for ${space}`} aria-label={`Actions for ${space}`} className={menuTrigger}>
          <EllipsisVertical className="size-4" aria-hidden />
        </MenuButton>
        <MenuItems anchor="bottom start" className={`${menuPanel} w-60`}>
          {role !== 'viewer' && (
            <>
              <MenuItem>
                <button type="button" onClick={() => openDeploy({ space })} className={menuItem}>
                  <Rocket className="size-4" aria-hidden /> Deploy an app here
                </button>
              </MenuItem>
              <MenuItem>
                <button type="button" onClick={() => setDialog('add')} className={menuItem}>
                  <PlusSquare className="size-4" aria-hidden /> Add a service…
                </button>
              </MenuItem>
              <div className="my-1 h-px bg-line" aria-hidden />
            </>
          )}
          <MenuItem>
            <button type="button" onClick={() => setDialog('traffic')} className={menuItem}>
              <Activity className="size-4" aria-hidden /> Traffic
            </button>
          </MenuItem>
          {role === 'admin' && (
            <>
              <MenuItem>
                <button type="button" onClick={() => setDialog('settings')} className={menuItem}>
                  <Settings className="size-4" aria-hidden /> Space settings
                </button>
              </MenuItem>
              <div className="my-1 h-px bg-line" aria-hidden />
              <MenuItem>
                <button
                  type="button"
                  onClick={() => setDialog('delete')}
                  className={`${menuItem} text-danger data-focus:bg-danger/10 data-focus:text-danger`}
                >
                  <Trash2 className="size-4" aria-hidden /> Delete space
                </button>
              </MenuItem>
            </>
          )}
        </MenuItems>
      </Menu>

      {dialog === 'add' && <AddServiceFlow space={space} onClose={close} />}
      {dialog === 'settings' && <SpaceSettingsDialog space={space} onClose={close} />}
      {dialog === 'delete' && <DeleteSpaceDialog space={space} apps={apps} onClose={close} />}
      <SpaceTrafficDrawer space={space} open={dialog === 'traffic'} onClose={close} />
    </>
  )
}

function SpaceSettingsDialog({ space, onClose }: { space: string; onClose: () => void }) {
  const queryClient = useQueryClient()
  return (
    <SpaceSettings
      space={space}
      onClose={onClose}
      onSaved={() => queryClient.invalidateQueries({ queryKey: fleetQuery.queryKey })}
    />
  )
}

// Without a type the dialog starts on the space's catalog.
function AddServiceFlow({ space, type, onClose }: { space: string; type?: string; onClose: () => void }) {
  const navigate = useNavigate()
  const { data: catalog } = useCatalog(space)
  const add = useAddService(space)
  const title = type ? catalog?.find((s) => s.type === type)?.title || type : 'a service'
  return (
    <AddServiceDialog
      space={space}
      open
      title={`Add ${title} to ${space}`}
      submitLabel="Add"
      busy={add.isPending}
      error={mutationError(add, 'Adding did not work.')}
      initialType={type}
      note={add.isPending ? 'Setting up — the first time can take a minute while the cluster downloads the image.' : ''}
      onSubmit={(req) =>
        add.mutate(req, {
          // The new service opens on its own page, ready to be used.
          onSuccess: () => {
            onClose()
            const name = req.name || req.type
            if (name) navigate({ to: '/service/$space/$name/{-$tab}', params: { space, name, tab: undefined } })
          },
        })
      }
      onClose={onClose}
    />
  )
}

/**
 * Deleting a space is the explicit cascade: it says exactly what goes —
 * every app, every service and its data — and wants the name typed back.
 */
function DeleteSpaceDialog({ space, apps, onClose }: { space: string; apps: AppSummary[]; onClose: () => void }) {
  const navigate = useNavigate()
  const params = useParams({ strict: false }) as { space?: string }
  const { data: services, error: servicesError } = useSpaceServices(space)
  const remove = useDeleteSpace(space)
  const [typed, setTyped] = useState('')
  const busy = remove.isPending

  return (
    <Modal onClose={() => !busy && onClose()} size="sm">
      <ModalHeader
        eyebrow="Delete space"
        title={
          <>
            Remove <span className="font-mono">{space}</span>
          </>
        }
        onClose={onClose}
        closeDisabled={busy}
      />
      <ModalNotice message={mutationError(remove, 'Removing did not work.')} />
      <ModalBody className="space-y-4">
        <Callout tone="danger" icon={TriangleAlert} title="This cannot be undone.">
          Every app disk and all managed-service data in the space is deleted. There is no keep-data option.
        </Callout>
        <dl className="space-y-2 text-sm">
          <div className="flex gap-3">
            <dt className="w-16 shrink-0 text-muted">Apps</dt>
            <dd className="min-w-0 font-mono text-ink">
              {apps.length > 0 ? (
                apps.map((app) => app.name).join(', ')
              ) : (
                <span className="font-sans text-muted">none</span>
              )}
            </dd>
          </div>
          <div className="flex gap-3">
            <dt className="w-16 shrink-0 text-muted">Services</dt>
            <dd className="min-w-0 font-mono text-ink">
              {services === null ? (
                <span className={`font-sans ${servicesError ? 'text-danger' : 'text-muted'}`}>
                  {servicesError
                    ? 'The inventory could not be loaded. The server still removes every owned service.'
                    : 'Loading…'}
                </span>
              ) : services.length > 0 ? (
                services.map((s) => s.name).join(', ')
              ) : (
                <span className="font-sans text-muted">none</span>
              )}
            </dd>
          </div>
        </dl>
        <Field
          label={
            <>
              Type <span className="normal-case tracking-normal text-ink">{space}</span> to confirm
            </>
          }
        >
          <Input autoFocus value={typed} onChange={(event) => setTyped(event.target.value)} disabled={busy} />
        </Field>
      </ModalBody>
      <ModalFooter>
        <Button kind="quiet" onClick={onClose} disabled={busy}>
          Cancel
        </Button>
        <Button
          kind="danger"
          disabled={busy || (services === null && !servicesError) || typed.trim() !== space}
          onClick={() =>
            remove.mutate(undefined, {
              onSuccess: () => {
                onClose()
                // The page that was open lived in the space that is gone.
                if (params.space === space) navigate({ to: '/' })
              },
            })
          }
        >
          {busy ? 'Removing…' : `Remove ${space}`}
        </Button>
      </ModalFooter>
    </Modal>
  )
}

/**
 * The one destructive act in the service world gets a proper stop: it says
 * who loses the service, and deleting the data is the default — keeping a
 * disk around is the deliberate choice, not the accident.
 */
export function RemoveServiceDialog({
  space,
  service,
  onClose,
  onRemoved,
}: {
  space: string
  service: SpaceAddonEntry
  onClose: () => void
  onRemoved?: () => void
}) {
  const [keepData, setKeepData] = useState(false)
  // A plugin-provided service whose plugin is gone or failing cannot be
  // removed properly; once the server says so, offer to drop donkey's record
  // and leave the backing service behind.
  const [orphan, setOrphan] = useState(false)
  const remove = useRemoveService(space)
  const busy = remove.isPending
  const error = mutationError(remove, 'Removing did not work.')
  const canOrphan = error.includes('--orphan')
  const isProxy = service.protocol === 'http'

  return (
    <Modal onClose={() => !busy && onClose()} size="sm">
      <ModalHeader
        eyebrow={`Remove service · ${space}`}
        title={<span className="font-mono">{service.name}</span>}
        onClose={onClose}
        closeDisabled={busy}
      />
      <ModalNotice message={error} />
      <ModalBody className="space-y-4">
        <div className="space-y-2 text-sm text-ink-soft">
          {service.attachedTo.length > 0 ? (
            <p>
              <span className="font-mono text-ink">{service.attachedTo.join(', ')}</span>{' '}
              {service.attachedTo.length === 1 ? 'is' : 'are'} connected —{' '}
              {service.attachedTo.length === 1 ? 'it' : 'they'} will restart without its variables.
            </p>
          ) : (
            <p>No app is connected to it.</p>
          )}
          {service.state && service.state !== 'ready' && (
            <p className="text-muted">
              It is {service.state}
              {service.message ? `: ${service.message}` : ''}.
            </p>
          )}
        </div>
        {!isProxy && !orphan && (
          <ToggleRow
            label="Keep its data"
            description="Adding a service with the same name later picks the data back up."
            checked={keepData}
            disabled={busy}
            onChange={setKeepData}
          />
        )}
        {canOrphan && (
          <ToggleRow
            label="Forget it anyway"
            description="Its plugin is not answering. Drop donkey's record and leave the backing service to the operator."
            checked={orphan}
            disabled={busy}
            onChange={(on) => {
              setOrphan(on)
              if (on) setKeepData(false)
            }}
          />
        )}
        {orphan ? (
          <Callout icon={Info}>
            The backing service and its data stay with the operator. Donkey forgets this instance.
          </Callout>
        ) : isProxy ? (
          <Callout icon={Info}>Removing this service revokes this space's access to the API.</Callout>
        ) : !keepData ? (
          <Callout tone="danger" icon={TriangleAlert}>
            Its disk and everything on it will be deleted.
          </Callout>
        ) : null}
      </ModalBody>
      <ModalFooter>
        <Button kind="quiet" onClick={onClose} disabled={busy}>
          Cancel
        </Button>
        <Button
          kind="danger"
          onClick={() =>
            remove.mutate(
              { name: service.name, keepData, orphan },
              {
                onSuccess: () => {
                  onClose()
                  onRemoved?.()
                },
              },
            )
          }
          disabled={busy}
        >
          {busy
            ? 'Removing…'
            : orphan
              ? `Forget ${service.name}`
              : keepData
                ? `Remove ${service.name}, keep data`
                : `Remove ${service.name} and its data`}
        </Button>
      </ModalFooter>
    </Modal>
  )
}

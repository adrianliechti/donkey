import { useState } from 'react'
import { Link2, Loader2, Unlink2 } from 'lucide-react'
import { api, type AddAddonRequest, type SpaceAddonEntry } from './api'
import { AddServiceDialog } from './AddonForm'
import { useAppServices, useInvalidateServices, useSpaceServices } from './queries'
import { serviceUI } from './serviceProtocol'
import { Chip, ErrorNote, errText, IconButton, List, ListRow, Section, SectionHeader, Spinner, StateChip, SuccessNote } from './ui'

/**
 * The services this app can use: every service of the space in one list,
 * each saying whether the app is connected and what it gets from it. One
 * action per row — Connect or Disconnect. Services belong to the space, so
 * disconnecting never deletes anything; a new one is made from the catalog.
 */
export function AddonsCard({ space, app }: { space: string; app: string }) {
  const [adding, setAdding] = useState(false)
  // The row whose request is running — its button shows the work.
  const [pending, setPending] = useState('')
  const [error, setError] = useState('')
  const [note, setNote] = useState('')

  const { data: addons, error: loadError } = useAppServices(space, app)
  const { data: available } = useSpaceServices(space)
  const invalidate = useInvalidateServices()

  const connected = new Map((addons ?? []).map((a) => [a.name, a]))
  // One row per space service, connected ones first. A connection whose
  // service is not in the space listing (attached by the CLI) still shows.
  const rows: { entry: SpaceAddonEntry; keys: string[]; on: boolean }[] = [
    ...(available ?? []).map((entry) => ({
      entry,
      keys: connected.get(entry.name)?.keys ?? entry.keys,
      on: connected.has(entry.name),
    })),
    ...(addons ?? [])
      .filter((a) => !(available ?? []).some((s) => s.name === a.name))
      .map((a) => ({ entry: { ...a, attachedTo: [app] } as SpaceAddonEntry, keys: a.keys, on: true })),
  ].sort((a, b) => Number(b.on) - Number(a.on) || a.entry.name.localeCompare(b.entry.name))

  async function run(name: string, work: () => Promise<{ ok?: boolean; restarted?: boolean } | void>) {
    setPending(name)
    setError('')
    setNote('')
    try {
      const res = await work()
      setAdding(false)
      setNote(
        res && res.restarted
          ? 'Done — the app is restarting with the change.'
          : 'Done — the app picks it up with its next deploy.',
      )
      invalidate(space)
    } catch (err) {
      setError(errText(err, 'That did not work.'))
    } finally {
      setPending('')
    }
  }

  const create = (req: AddAddonRequest) => run(req.name ?? req.type ?? 'new', () => api.addAddon(space, app, req))
  const loading = addons === null && !loadError

  return (
    <Section className="space-y-3">
      <SectionHeader
        title="Services"
        description="Connecting injects the service's variables and restarts the app. Services belong to the space — disconnecting deletes nothing."
        actions={
          <button
            type="button"
            onClick={() => setAdding(true)}
            className="rounded px-1 text-xs text-muted underline-offset-2 transition-colors hover:text-ink hover:underline focus-visible:outline-2 focus-visible:outline-accent-strong"
          >
            New service
          </button>
        }
      />
      <ErrorNote message={error || loadError} />
      {loading && <Spinner label="Loading…" />}
      {!loading && rows.length === 0 && (
        <p className="text-sm text-muted">
          This space has no services yet. Make one with New service — it will be connected to {app}.
        </p>
      )}
      {rows.length > 0 && (
        // The same quiet table as the variables: one row per service, the
        // variables it brings as chips, and the one action as an icon.
        <List>
          {rows.map(({ entry, keys, on }) => {
            const { Icon } = serviceUI(entry.protocol)
            const unready = Boolean(entry.state && entry.state !== 'ready')
            const busy = pending === entry.name
            return (
              <ListRow
                key={entry.name}
                actions={
                  busy ? (
                    <span className="flex size-7 items-center justify-center text-muted">
                      <Loader2 className="size-3.5 animate-spin" aria-hidden />
                    </span>
                  ) : on ? (
                    <IconButton
                      icon={Unlink2}
                      size="sm"
                      kind="danger"
                      label={`Disconnect ${entry.name}`}
                      disabled={pending !== ''}
                      onClick={() => run(entry.name, () => api.removeAddon(space, app, entry.name))}
                    />
                  ) : (
                    <IconButton
                      icon={Link2}
                      size="sm"
                      label={`Connect ${entry.name}`}
                      disabled={pending !== '' || unready}
                      onClick={() => run(entry.name, () => api.addAddon(space, app, { name: entry.name }))}
                    />
                  )
                }
              >
                <Icon className={`size-4 shrink-0 ${on ? 'text-ink-soft' : 'text-muted/60'}`} aria-hidden />
                <div className="flex min-w-0 flex-1 flex-wrap items-center gap-x-3 gap-y-1">
                  <span className={`font-mono text-sm ${on ? 'text-ink' : 'text-muted'}`}>{entry.name}</span>
                  {entry.type && entry.type !== entry.name && (
                    <span className="font-mono text-[11px] text-muted">{entry.type}</span>
                  )}
                  {unready && <StateChip state={entry.state!} />}
                  {!on && !unready && <span className="text-xs text-muted">not connected</span>}
                  {keys.length > 0 && (
                    <span className="flex flex-wrap gap-1" title={on ? 'Variables the app receives' : 'Variables the app would receive'}>
                      {keys.map((k) => <Chip key={k} dashed={!on}>{k}</Chip>)}
                    </span>
                  )}
                </div>
              </ListRow>
            )
          })}
        </List>
      )}
      <SuccessNote message={note} />
      <AddServiceDialog
        space={space}
        open={adding}
        title="New service"
        submitLabel="Create and connect"
        busy={pending !== ''}
        error={error}
        note={pending ? 'Setting up — the first time can take a minute while the cluster downloads the image.' : ''}
        onSubmit={create}
        onClose={() => setAdding(false)}
      />
    </Section>
  )
}

import { useState } from 'react'
import { Plus } from 'lucide-react'
import { api, type AddAddonRequest } from './api'
import { AddServiceDialog } from './AddonForm'
import { useAppServices, useInvalidateServices, useSpaceServices } from './queries'
import { Button, ErrorNote, errText, SectionTitle, Spinner } from './ui'

/**
 * The services this app is connected to. Services belong to the space —
 * disconnecting here never deletes anything, and the space's other services
 * are one click away.
 */
export function AddonsCard({ space, app }: { space: string; app: string }) {
  const [adding, setAdding] = useState(false)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const [note, setNote] = useState('')

  const { data: addons, error: loadError } = useAppServices(space, app)
  const { data: available } = useSpaceServices(space)
  const invalidate = useInvalidateServices()

  const connected = new Set((addons ?? []).map((a) => a.name))
  const others = (available ?? []).filter((s) => !connected.has(s.name))

  async function run(work: () => Promise<{ ok?: boolean; restarted?: boolean } | void>, doing: string) {
    setBusy(true)
    setError('')
    setNote(doing)
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
      setNote('')
      setError(errText(err, 'That did not work.'))
    } finally {
      setBusy(false)
    }
  }

  const create = (req: AddAddonRequest) =>
    run(
      () => api.addAddon(space, app, req),
      req.external ? '' : 'Setting up — the first time can take a minute while the cluster downloads the image.',
    )

  return (
    <section className="border-t border-line pt-5">
      <div className="flex items-center justify-between gap-2">
        <SectionTitle>Connected services</SectionTitle>
        <Button kind="quiet" onClick={() => setAdding(true)}>
          <span className="flex items-center gap-1">
            <Plus className="size-3.5" aria-hidden /> Add a service
          </span>
        </Button>
      </div>
      <ErrorNote message={error || loadError} />
      {note && <p className="mt-2 text-xs text-ink-soft">{note}</p>}
      {addons === null && !error && <Spinner label="Loading…" />}
      {addons !== null && addons.length === 0 && (
        <p className="mt-2 text-sm text-stone">Not connected to any service yet.</p>
      )}
      <ul className="mt-2 space-y-3">
        {(addons ?? []).map((a) => (
          <li key={a.name} className="border-b border-line/40 pb-3 last:border-0 last:pb-0">
            {/* The button holds its corner whatever the key list does below —
                nine postgres keys and one URL lay out the same way. */}
            <div className="flex items-center justify-between gap-2">
              <span className="font-mono text-sm font-medium">{a.name}</span>
              <Button
                kind="quiet"
                disabled={busy}
                onClick={() => run(() => api.removeAddon(space, app, a.name), '')}
              >
                Disconnect
              </Button>
            </div>
            <div className="mt-1.5 flex flex-wrap gap-1">
              {a.keys.map((k) => (
                <span
                  key={k}
                  className="rounded bg-raised px-1.5 py-0.5 font-mono text-[10px] text-stone"
                >
                  {k}
                </span>
              ))}
            </div>
          </li>
        ))}
      </ul>
      {others.length > 0 && (
        <div className="mt-3 flex flex-wrap items-center gap-2 border-t border-line pt-3">
          <span className="text-xs text-stone">Also in this space:</span>
          {others.map((s) => (
            <Button
              key={s.name}
              kind="quiet"
              disabled={busy}
              onClick={() => run(() => api.addAddon(space, app, { name: s.name }), '')}
            >
              <span className="font-mono text-xs">Connect {s.name}</span>
            </Button>
          ))}
        </div>
      )}
      <AddServiceDialog
        open={adding}
        title="Add a service"
        submitLabel="Add and connect"
        busy={busy}
        error={error}
        note={busy ? note : ''}
        onSubmit={create}
        onClose={() => setAdding(false)}
      />
      <p className="mt-3 text-xs text-stone">
        The variables above are injected into the app. Services belong to the space — disconnecting
        deletes nothing, and the space card manages the services themselves.
      </p>
    </section>
  )
}

import { useState } from 'react'
import { TriangleAlert } from 'lucide-react'
import { api } from './api'
import { Button, ErrorNote, errText } from './ui'

/**
 * Removing apps from the overview.
 *
 * The unit people think in is not always one app. A space holds the apps that
 * belong together — a frontend and the backend it talks to — and they are
 * related in a way the cluster enforces: they share a namespace and reach each
 * other by plain name. Removing the backend of a pair leaves the frontend up
 * and quietly broken, so this dialog takes a *list* of apps and is used for
 * both cases: one app, or every app in a space.
 *
 * Deletes run one at a time rather than all at once. A half-finished removal
 * is a state someone has to clean up by hand, so it matters more that the
 * report afterwards is accurate than that it finishes quickly.
 */
export function RemoveApps({
  space,
  apps,
  siblings,
  onDone,
  onCancel,
}: {
  space: string
  apps: string[]
  /** Other apps left behind in the space, which may reach this one by name. */
  siblings: string[]
  onDone: () => void
  onCancel: () => void
}) {
  const [keepData, setKeepData] = useState(false)
  const [busy, setBusy] = useState(false)
  const [typed, setTyped] = useState('')
  const [error, setError] = useState('')
  const [done, setDone] = useState<string[]>([])

  const whole = apps.length > 1
  // Nothing else would be left behind — so this is also the end of the space.
  const last = siblings.length === 0
  // Removing a whole space is not something to do by mis-clicking a button
  // that sits next to "open". Typing the name is the one place a confirmation
  // step earns its keep.
  const confirmed = !whole || typed.trim() === space

  async function remove() {
    setBusy(true)
    setError('')
    const removed: string[] = []
    for (const app of apps) {
      try {
        await api.destroy(space, app, keepData)
        removed.push(app)
        setDone([...removed])
      } catch (err) {
        setError(
          errText(err, `Removing ${app} did not work.`) +
            (removed.length ? ` ${removed.join(', ')} ${removed.length === 1 ? 'was' : 'were'} already removed.` : ''),
        )
        setBusy(false)
        return
      }
    }
    onDone()
  }

  return (
    <div
      className="fixed inset-0 z-50 flex items-center justify-center bg-ink/40 p-4"
      role="dialog"
      aria-modal="true"
      onClick={(e) => {
        if (e.target === e.currentTarget && !busy) onCancel()
      }}
    >
      <div className="w-full max-w-md rounded-lg border border-line bg-card p-4 shadow-lg">
        <p className="flex items-center gap-2 text-sm font-medium text-brick">
          <TriangleAlert className="size-4" aria-hidden />
          {whole ? `Remove all ${apps.length} apps in ${space}?` : `Remove ${apps[0]}?`}
        </p>

        <p className="mt-2 text-sm text-ink-soft">
          {whole ? (
            <>
              This takes <span className="font-mono">{apps.join(', ')}</span> off the cluster.
            </>
          ) : (
            <>
              This takes <span className="font-mono">{apps[0]}</span> and everything donkey created for it off the
              cluster. A later <code className="font-mono text-xs">donkey deploy</code> brings it back.
            </>
          )}{' '}
          {/* The namespace is the space's home, and it only goes when the last
              app leaves *and* nothing was kept: data someone chose to keep
              would go with it. Saying "and the space with them" regardless
              would be a promise that quietly stops being true the moment the
              box below is ticked. */}
          {last &&
            (keepData ? (
              <>
                The <span className="font-mono">{space}</span> space stays, because the data kept below lives in it.
              </>
            ) : (
              <>
                Nothing else is left in <span className="font-mono">{space}</span>, so the space goes too.
              </>
            ))}
        </p>

        {/* The failure this prevents is silent: the app that stays up keeps
            answering, and only the calls to the one that left start failing. */}
        {siblings.length > 0 && (
          <p className="mt-2 rounded-md border border-line bg-canvas px-3 py-2 text-xs text-ink-soft">
            {siblings.length === 1 ? (
              <>
                <span className="font-mono">{siblings[0]}</span> stays in this space and may reach{' '}
                <span className="font-mono">{apps[0]}</span> as{' '}
                <code className="font-mono">http://{apps[0]}</code>. It will keep running, and those calls will fail.
              </>
            ) : (
              <>
                {siblings.length} apps stay in this space (<span className="font-mono">{siblings.join(', ')}</span>) and
                may reach <span className="font-mono">{apps[0]}</span> by name. They will keep running, and those calls
                will fail.
              </>
            )}
          </p>
        )}

        <label className="mt-3 flex items-center gap-2 text-sm text-ink-soft">
          <input
            type="checkbox"
            checked={keepData}
            onChange={(e) => setKeepData(e.target.checked)}
            className="size-4 accent-hay-deep"
            disabled={busy}
          />
          Keep stored data (databases, disks)
        </label>

        {whole && (
          <label className="mt-3 block text-sm text-ink-soft">
            Type <span className="font-mono font-bold">{space}</span> to confirm
            <input
              value={typed}
              onChange={(e) => setTyped(e.target.value)}
              disabled={busy}
              autoFocus
              className="mt-1 w-full rounded-md border border-line bg-canvas px-2 py-1 font-mono text-sm"
            />
          </label>
        )}

        {busy && whole && (
          <p className="mt-3 text-xs text-stone">
            Removed {done.length} of {apps.length}…
          </p>
        )}

        <div className="mt-4 flex gap-2">
          <Button kind="danger" onClick={remove} disabled={busy || !confirmed}>
            {busy ? 'Removing…' : keepData ? 'Remove, keep data' : 'Remove and delete data'}
          </Button>
          <Button kind="quiet" onClick={onCancel} disabled={busy}>
            Cancel
          </Button>
        </div>
        <ErrorNote message={error} />
      </div>
    </div>
  )
}

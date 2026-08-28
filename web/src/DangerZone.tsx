import { useState } from 'react'
import { Trash2, TriangleAlert } from 'lucide-react'
import { api } from './api'
import { Button, ErrorNote, errText, SectionTitle } from './ui'

export function DangerZone({ space, app, onDestroyed }: { space: string; app: string; onDestroyed: () => void }) {
  const [confirming, setConfirming] = useState(false)
  const [keepData, setKeepData] = useState(false)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')

  async function destroy() {
    setBusy(true)
    setError('')
    try {
      await api.destroy(space, app, keepData)
      onDestroyed()
    } catch (err) {
      setError(errText(err, 'Removing did not work.'))
      setBusy(false)
    }
  }

  return (
    <section className="border-t border-brick/30 pt-5">
      <SectionTitle>Remove</SectionTitle>
      {!confirming ? (
        <div className="mt-2 flex items-center justify-between gap-4">
          <p className="text-sm text-ink-soft">
            Takes <span className="font-mono">{app}</span> off the cluster. A later{' '}
            <code className="font-mono text-xs">donkey deploy</code> brings it back.
          </p>
          <Button kind="danger" onClick={() => setConfirming(true)}>
            <span className="flex items-center gap-1.5">
              <Trash2 className="size-3.5" aria-hidden /> Remove app
            </span>
          </Button>
        </div>
      ) : (
        <div className="mt-2 space-y-3">
          <p className="flex items-center gap-2 text-sm font-medium text-brick">
            <TriangleAlert className="size-4" aria-hidden />
            Remove {app} and everything donkey created for it?
          </p>
          <label className="flex items-center gap-2 text-sm text-ink-soft">
            <input
              type="checkbox"
              checked={keepData}
              onChange={(e) => setKeepData(e.target.checked)}
              className="size-4 accent-hay-deep"
            />
            Keep its disk (the space's services are never touched)
          </label>
          <div className="flex gap-2">
            <Button kind="danger" onClick={destroy} disabled={busy}>
              {busy ? 'Removing…' : keepData ? 'Remove app, keep its data' : 'Remove app and its data'}
            </Button>
            <Button kind="quiet" onClick={() => setConfirming(false)} disabled={busy}>
              Cancel
            </Button>
          </div>
          <ErrorNote message={error} />
        </div>
      )}
    </section>
  )
}

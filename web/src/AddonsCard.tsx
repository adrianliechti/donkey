import { useCallback, useEffect, useState } from 'react'
import { api, type AddonEntry } from './api'
import { Button, Card, ErrorNote, errText, SectionTitle, Spinner } from './ui'

export function AddonsCard({ space, app }: { space: string; app: string }) {
  const [addons, setAddons] = useState<AddonEntry[] | null>(null)
  const [removing, setRemoving] = useState('')
  const [keepData, setKeepData] = useState(true)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')

  const load = useCallback(() => {
    api
      .addons(space, app)
      .then((res) => setAddons(res.addons ?? []))
      .catch((err) => setError(errText(err, 'Loading failed.')))
  }, [space, app])

  useEffect(() => {
    load()
  }, [load])

  async function remove(name: string) {
    setBusy(true)
    setError('')
    try {
      await api.removeAddon(space, app, name, keepData)
      setRemoving('')
      load()
    } catch (err) {
      setError(errText(err, 'Removing did not work.'))
    } finally {
      setBusy(false)
    }
  }

  if (addons !== null && addons.length === 0 && !error) return null

  return (
    <Card className="p-4">
      <SectionTitle>Attached services</SectionTitle>
      <ErrorNote message={error} />
      {addons === null && !error && <Spinner label="Loading…" />}
      <ul className="mt-2 space-y-2">
        {(addons ?? []).map((a) => (
          <li key={a.name} className="flex flex-wrap items-center justify-between gap-2">
            <div className="min-w-0">
              <span className="font-mono text-sm font-medium">{a.name}</span>
              <span className="ml-2 font-mono text-xs text-stone">{a.keys.join(' · ')}</span>
            </div>
            {removing === a.name ? (
              <div className="flex items-center gap-2">
                <label className="flex items-center gap-1.5 text-xs text-ink-soft">
                  <input
                    type="checkbox"
                    checked={keepData}
                    onChange={(e) => setKeepData(e.target.checked)}
                    className="size-3.5 accent-hay-deep"
                  />
                  Keep its data
                </label>
                <Button kind="danger" onClick={() => remove(a.name)} disabled={busy}>
                  {busy ? 'Removing…' : keepData ? 'Remove, keep data' : 'Remove with data'}
                </Button>
                <Button kind="quiet" onClick={() => setRemoving('')} disabled={busy}>
                  Cancel
                </Button>
              </div>
            ) : (
              <Button kind="quiet" onClick={() => setRemoving(a.name)}>
                Remove
              </Button>
            )}
          </li>
        ))}
      </ul>
      <p className="mt-3 text-xs text-stone">
        The variables above are injected into the app. Add services from the project folder:{' '}
        <code className="font-mono">donkey addon add postgres</code>
      </p>
    </Card>
  )
}

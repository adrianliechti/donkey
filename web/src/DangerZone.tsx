import { useState } from 'react'
import { useQueryClient } from '@tanstack/react-query'
import { Trash2, TriangleAlert } from 'lucide-react'
import { api } from './api'
import { Button, Callout, ErrorNote, IconButton, Section, SectionHeader, ToggleRow } from './ui'
import { errText } from './format'
import { deploysQuery, fleetQuery, statusQuery } from './queries'

export function DangerZone({ space, app, onDestroyed }: { space: string; app: string; onDestroyed: () => void }) {
  const queries = useQueryClient()
  const [confirming, setConfirming] = useState(false)
  const [keepData, setKeepData] = useState(false)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')

  async function destroy() {
    setBusy(true)
    setError('')
    try {
      await api.destroy(space, app, keepData)
      await Promise.all([
        queries.invalidateQueries({ queryKey: fleetQuery.queryKey }),
        queries.invalidateQueries({ queryKey: deploysQuery.queryKey }),
        queries.invalidateQueries({ queryKey: statusQuery(space, app).queryKey }),
      ])
      onDestroyed()
    } catch (err) {
      setError(errText(err, 'Removing did not work.'))
      setBusy(false)
    }
  }

  return (
    <Section tone="danger" className="space-y-3">
      <SectionHeader
        title="Remove"
        actions={
          !confirming && (
            <IconButton icon={Trash2} kind="danger" label="Remove app…" onClick={() => setConfirming(true)} />
          )
        }
        description={
          <>
            Takes <span className="font-mono">{app}</span> off the cluster. A later{' '}
            <code className="font-mono">donkey deploy</code> brings it back.
          </>
        }
      />
      {confirming && (
        <>
          <Callout tone="danger" icon={TriangleAlert} title={`Remove ${app} and everything donkey created for it?`}>
            The space's services are never touched.
          </Callout>
          <ToggleRow
            label="Keep its data"
            description="Its disk, build history and source history stay; redeploying later picks them back up."
            checked={keepData}
            onChange={setKeepData}
          />
          <ErrorNote message={error} />
          <div className="flex justify-end gap-2">
            <Button kind="quiet" onClick={() => setConfirming(false)} disabled={busy}>
              Cancel
            </Button>
            <Button kind="danger" onClick={destroy} disabled={busy}>
              {busy ? 'Removing…' : keepData ? 'Remove app, keep its data' : 'Remove app and its data'}
            </Button>
          </div>
        </>
      )}
    </Section>
  )
}

import { useState } from 'react'
import { Trash2, TriangleAlert } from 'lucide-react'
import { api } from './api'
import { Button, Callout, ErrorNote, errText, IconButton, Section, SectionHeader, ToggleRow } from './ui'

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
          <ToggleRow label="Keep its disk" description="Redeploying later picks the data back up." checked={keepData} onChange={setKeepData} />
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

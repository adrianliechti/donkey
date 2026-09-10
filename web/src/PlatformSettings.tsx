import { useEffect, useState } from 'react'
import { Dialog, DialogBackdrop, DialogPanel, DialogTitle } from '@headlessui/react'
import { useQueryClient } from '@tanstack/react-query'
import { api, type PlatformConfig } from './api'
import { fleetQuery } from './queries'
import { MembershipEditor } from './MembershipEditor'
import { Button, ErrorNote, Spinner, errText } from './ui'

export function PlatformSettings({ onClose }: { onClose: () => void }) {
  const queryClient = useQueryClient()
  const [settings, setSettings] = useState<PlatformConfig | null>(null)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')

  useEffect(() => {
    let active = true
    api.platformConfig().then((value) => { if (active) setSettings(value) })
      .catch((err) => { if (active) setError(errText(err, 'Loading platform settings failed.')) })
    return () => { active = false }
  }, [])

  async function save() {
    if (!settings) return
    setBusy(true)
    setError('')
    try {
      await api.setPlatformConfig(settings)
      await queryClient.invalidateQueries({ queryKey: fleetQuery.queryKey })
      onClose()
    } catch (err) {
      setError(errText(err, 'Saving platform settings failed.'))
    } finally {
      setBusy(false)
    }
  }

  return (
    <Dialog open onClose={() => !busy && onClose()} className="relative z-50">
      <DialogBackdrop className="fixed inset-0 bg-black/60" />
      <div className="fixed inset-0 overflow-y-auto p-4 pt-[10vh]">
        <DialogPanel className="mx-auto w-full max-w-lg rounded-xl border border-line bg-card p-6 shadow-2xl">
          <DialogTitle className="mb-4 text-base font-semibold">Platform settings</DialogTitle>
          <ErrorNote message={error} />
          {!settings && !error && <Spinner label="Loading platform settings…" />}
          <fieldset disabled={busy} className="space-y-3">
            <p className="text-sm text-muted">Platform admins manage all spaces and can add other admins.</p>
            {settings && <MembershipEditor admins={settings.admins} onChange={(admins) => setSettings({ admins })} />}
            <p className="text-xs text-muted">Keep yourself as an admin when saving.</p>
          </fieldset>
          <div className="mt-5 flex justify-end gap-2">
            <Button kind="quiet" onClick={onClose} disabled={busy}>Cancel</Button>
            <Button onClick={save} disabled={!settings || busy}>{busy ? 'Saving…' : 'Save settings'}</Button>
          </div>
        </DialogPanel>
      </div>
    </Dialog>
  )
}

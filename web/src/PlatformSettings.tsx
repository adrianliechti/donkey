import { useEffect, useState } from 'react'
import { useQueryClient } from '@tanstack/react-query'
import { api, type PlatformConfig } from './api'
import { fleetQuery } from './queries'
import { MembershipEditor } from './MembershipEditor'
import { Modal, ModalBody, ModalFooter, ModalHeader, ModalNotice } from './dialog'
import { Button, Spinner } from './ui'
import { errText } from './format'

export function PlatformSettings({ onClose }: { onClose: () => void }) {
  const queryClient = useQueryClient()
  const [settings, setSettings] = useState<PlatformConfig | null>(null)
  const [saved, setSaved] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const dirty = settings !== null && JSON.stringify(settings) !== saved

  useEffect(() => {
    let active = true
    api
      .platformConfig()
      .then((value) => {
        if (active) {
          setSettings(value)
          setSaved(JSON.stringify(value))
        }
      })
      .catch((err) => {
        if (active) setError(errText(err, 'Loading platform settings failed.'))
      })
    return () => {
      active = false
    }
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
    <Modal onClose={() => !busy && onClose()} size="md">
      <ModalHeader eyebrow="Platform" title="Platform settings" onClose={onClose} closeDisabled={busy} />
      <ModalNotice message={error} />
      <ModalBody>
        {!settings && !error && <Spinner label="Loading platform settings…" />}
        {settings && (
          <fieldset disabled={busy} className="space-y-3">
            <p className="text-sm text-ink-soft">Platform admins manage all spaces and can add other admins.</p>
            <MembershipEditor admins={settings.admins} onChange={(admins) => setSettings({ admins })} />
            <p className="text-xs text-muted">Keep yourself as an admin when saving.</p>
          </fieldset>
        )}
      </ModalBody>
      <ModalFooter>
        <Button kind="quiet" onClick={onClose} disabled={busy}>
          Cancel
        </Button>
        <Button onClick={save} disabled={!dirty || busy}>
          {busy ? 'Saving…' : 'Save settings'}
        </Button>
      </ModalFooter>
    </Modal>
  )
}

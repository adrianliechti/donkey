import { useEffect, useState } from 'react'
import { useQueryClient } from '@tanstack/react-query'
import { api, type PlatformConfig } from './api'
import { fleetQuery } from './queries'
import { MembershipEditor } from './MembershipEditor'
import { Modal, ModalBody, ModalFooter, ModalHeader, ModalNotice } from './dialog'
import { Button, Field, Section, SectionHeader, Select, Spinner } from './ui'
import { errText } from './format'

// How old a running build may get before its app is labeled unmaintained.
const DEFAULT_UNMAINTAINED_DAYS = 90
const unmaintainedChoices = [30, 60, 90, 180, 365]

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
          <fieldset disabled={busy} className="space-y-6">
            <Section className="space-y-3">
              <SectionHeader
                title="Platform admins"
                description="Platform admins manage all spaces and can add other admins. Keep yourself as an admin when saving."
              />
              <MembershipEditor admins={settings.admins} onChange={(admins) => setSettings({ ...settings, admins })} />
            </Section>
            <Section className="space-y-3">
              <SectionHeader
                title="Unmaintained apps"
                description="Apps whose running build is older than this are labeled unmaintained."
              />
              <UnmaintainedAfter
                days={settings.unmaintainedAfterDays || DEFAULT_UNMAINTAINED_DAYS}
                disabled={busy}
                onChange={(days) => setSettings({ ...settings, unmaintainedAfterDays: days })}
              />
            </Section>
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

function UnmaintainedAfter({
  days,
  disabled,
  onChange,
}: {
  days: number
  disabled: boolean
  onChange: (days: number) => void
}) {
  // A value saved outside these choices stays selectable.
  const choices = unmaintainedChoices.includes(days)
    ? unmaintainedChoices
    : [...unmaintainedChoices, days].sort((a, b) => a - b)
  return (
    <Field label="Unmaintained after">
      <Select
        value={days}
        onChange={onChange}
        ariaLabel="Unmaintained after"
        disabled={disabled}
        className="w-40"
        options={choices.map((value) => ({
          value,
          label: value === 365 ? '1 year' : `${value} days`,
        }))}
      />
    </Field>
  )
}

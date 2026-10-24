import { useState } from 'react'
import type { SpaceConfig } from './api'
import { Modal, ModalBody, ModalFooter, ModalHeader, ModalNotice } from './dialog'
import { mutationError, useCreateSpace } from './queries'
import { SpaceForm } from './SpaceSettings'
import { Button } from './ui'

/**
 * A space usually appears by deploying into it; this makes one ahead of
 * time — a home for services (or teammates) before any app arrives.
 */
export function NewSpaceDialog({
  domain,
  open,
  onClose,
  onCreated,
}: {
  domain: string
  open: boolean
  onClose: () => void
  onCreated: (space: string) => void
}) {
  const [name, setName] = useState('')
  const [reserved, setReserved] = useState(false)
  const [settings, setSettings] = useState<SpaceConfig>({ admins: {}, developers: {}, viewers: {}, allowPublic: false, domains: [] })
  const [customDomains, setCustomDomains] = useState(false)
  const create = useCreateSpace()
  const busy = create.isPending
  const trimmed = name.trim()
  const defaults = trimmed && domain ? [`${trimmed}.${domain}`] : []
  const effective = { ...settings, domains: customDomains ? settings.domains : defaults }
  const validName = /^[a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?$/.test(trimmed)
  const error = reserved
    ? 'This name is reserved for Donkey. Choose another space name.'
    : mutationError(create, 'Creating did not work.')

  function submit() {
    if (['donkey', 'platform', 'system'].includes(trimmed)) {
      setReserved(true)
      return
    }
    setReserved(false)
    create.mutate({ name: trimmed, settings: effective }, { onSuccess: () => onCreated(trimmed) })
  }

  // The same frame, tabs and height as Space settings: a space looks the
  // same whether it is being made or changed.
  return (
    <Modal open={open} onClose={() => !busy && onClose()} size="lg" height="medium">
      <ModalHeader
        eyebrow="New space"
        title={trimmed ? <span className="font-mono">{trimmed}</span> : 'Create space'}
        onClose={onClose}
        closeDisabled={busy}
      />
      <ModalNotice message={error} />
      <ModalBody padded={false}>
        <SpaceForm
          settings={effective}
          disabled={busy}
          name={{
            value: name,
            onChange: setName,
            domainPlaceholder: !customDomains && !trimmed && domain ? `<space>.${domain}` : undefined,
          }}
          onChange={(next) => {
            if (next.domains !== effective.domains) setCustomDomains(true)
            setSettings(next)
          }}
        />
      </ModalBody>
      <ModalFooter note="A home for your apps and services.">
        <Button kind="quiet" onClick={onClose} disabled={busy}>
          Cancel
        </Button>
        <Button onClick={submit} disabled={busy || !validName}>
          {busy ? 'Creating…' : 'Create space'}
        </Button>
      </ModalFooter>
    </Modal>
  )
}

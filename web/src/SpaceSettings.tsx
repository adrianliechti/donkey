import { useEffect, useState } from 'react'
import { Dialog, DialogBackdrop, DialogPanel, DialogTitle } from '@headlessui/react'
import { api, type AccessConfig, type SpaceConfig } from './api'
import { Button, ErrorNote, Spinner, errText } from './ui'

function Entries({ label, values, placeholder, onChange }: {
  label: string; values: string[]; placeholder: string; onChange: (values: string[]) => void
}) {
  const value = values.join('\n')
  const [draft, setDraft] = useState({ value, text: value })
  if (value !== draft.value) setDraft({ value, text: value })
  return (
    <label className="block text-xs text-muted">
      {label}
      <textarea rows={2} value={draft.text} placeholder={placeholder} spellCheck={false}
        onChange={(e) => {
          const next = [...new Set(e.target.value.split(/[\n,]+/).map((s) => s.trim()).filter(Boolean))]
          setDraft({ value: next.join('\n'), text: e.target.value })
          onChange(next)
        }}
        className="mt-1 block w-full rounded-md border border-line bg-surface px-2 py-1.5 font-mono text-sm text-ink focus:outline-2 focus:outline-accent-strong" />
    </label>
  )
}

export function DomainSettings({ domains, onChange }: { domains: string[]; onChange: (domains: string[]) => void }) {
  return (
    <div className="space-y-1">
      <Entries label="Domains" values={domains} placeholder="team.example.com" onChange={onChange} />
      <p className="text-xs text-muted">Apps may use these domains and their subdomains. One per line. Clear the list to allow any domain.</p>
    </div>
  )
}

export function Members({ label, hint, members, onChange }: {
  label: string; hint: string; members: AccessConfig; onChange: (members: AccessConfig) => void
}) {
  return (
    <fieldset className="space-y-2 border-t border-line pt-3">
      <legend className="px-1 text-sm font-medium">{label}</legend>
      <p className="text-xs text-muted">{hint}</p>
      <div className="grid gap-2 sm:grid-cols-3">
        <Entries label="Users" values={members.users ?? []} placeholder="ada@example.com" onChange={(users) => onChange({ ...members, users })} />
        <Entries label="Groups" values={members.groups ?? []} placeholder="engineering" onChange={(groups) => onChange({ ...members, groups })} />
        <Entries label="Identity-provider roles" values={members.roles ?? []} placeholder="team-developer" onChange={(roles) => onChange({ ...members, roles })} />
      </div>
    </fieldset>
  )
}

export function SpaceConfigEditor({ settings, onChange }: {
  settings: SpaceConfig; onChange: (settings: SpaceConfig) => void
}) {
  return (
    <div className="space-y-4">
      <DomainSettings domains={settings.domains ?? []} onChange={(domains) => onChange({ ...settings, domains })} />
      <Members label="Space admins" hint="Manage membership, domains, and the entire space, including deletion."
        members={settings.admins} onChange={(admins) => onChange({ ...settings, admins })} />
      <Members label="Developers" hint="Deploy and manage apps and services, including their data and credentials."
        members={settings.developers} onChange={(developers) => onChange({ ...settings, developers })} />
      <p className="text-xs text-muted">Entries match your sign-in provider. Matching any entry grants the role; empty lists grant nobody access. Users are email addresses, or sub:subject when email is unavailable. Platform admins always retain access.</p>
    </div>
  )
}

export function SpaceSettings({ space, onClose, onSaved }: {
  space: string; onClose: () => void; onSaved: () => void
}) {
  const [settings, setSettings] = useState<SpaceConfig | null>(null)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  useEffect(() => {
    let active = true
    api.spaceConfig(space).then((value) => { if (active) setSettings(value) })
      .catch((err) => { if (active) setError(errText(err, 'Loading space settings failed.')) })
    return () => { active = false }
  }, [space])

  async function save() {
    if (!settings) return
    setBusy(true)
    setError('')
    try {
      await api.setSpaceConfig(space, settings)
      onSaved()
      onClose()
    } catch (err) {
      setError(errText(err, 'Saving space settings failed.'))
    } finally {
      setBusy(false)
    }
  }

  return (
    <Dialog open onClose={() => !busy && onClose()} className="relative z-50">
      <DialogBackdrop className="fixed inset-0 bg-black/60" />
      <div className="fixed inset-0 overflow-y-auto p-4 pt-[10vh]">
        <DialogPanel className="mx-auto w-full max-w-2xl rounded-lg border border-line bg-card p-5 shadow-2xl">
          <DialogTitle className="mb-4 text-base font-semibold">Settings for {space}</DialogTitle>
          <ErrorNote message={error} />
          {!settings && !error && <Spinner label="Loading space settings…" />}
          <fieldset disabled={busy}>
            {settings && <SpaceConfigEditor settings={settings} onChange={setSettings} />}
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

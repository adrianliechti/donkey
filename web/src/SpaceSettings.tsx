import { useEffect, useState } from 'react'
import { Dialog, DialogBackdrop, DialogPanel, DialogTitle } from '@headlessui/react'
import { useQueryClient } from '@tanstack/react-query'
import { ChevronRight, Globe2, Plus, Users, X } from 'lucide-react'
import { api, type SpaceConfig } from './api'
import { MembershipEditor } from './MembershipEditor'
import { OfferingSettings } from './OfferingSettings'
import { Button, ErrorNote, Spinner, errText } from './ui'

export function DomainSettings({ domains, onChange, placeholder }: {
  domains: string[]; onChange: (domains: string[]) => void; placeholder?: string
}) {
  const [adding, setAdding] = useState(false)
  const [draft, setDraft] = useState('')
  function add() {
    const domain = draft.trim().toLowerCase()
    if (!domain) return
    onChange([...new Set([...domains, domain])])
    setDraft('')
    setAdding(false)
  }
  return (
    <section aria-label="Domains" className="space-y-2">
      <div className="flex items-center justify-between">
        <h3 className="flex items-center gap-2 text-sm font-medium"><Globe2 className="size-4 text-muted" aria-hidden /> Domains</h3>
        <button type="button" onClick={() => setAdding(!adding)} className="flex items-center gap-1 rounded px-1 py-0.5 text-xs text-accent hover:bg-accent-soft">
          <Plus className="size-3.5" aria-hidden /> Add domain
        </button>
      </div>
      <div className="flex min-h-10 flex-wrap items-center gap-2 rounded-lg border border-line bg-surface/50 p-2">
        {domains.map((domain) => <span key={domain} className="inline-flex max-w-full items-center gap-1.5 rounded-md border border-line bg-card py-1 pl-2.5 pr-1 text-xs">
          <span className="truncate font-mono" title={domain}>{domain}</span>
          <button type="button" aria-label={`Remove domain ${domain}`} onClick={() => onChange(domains.filter((d) => d !== domain))}
            className="shrink-0 rounded p-0.5 text-muted hover:bg-raised hover:text-ink"><X className="size-3" aria-hidden /></button>
        </span>)}
        {domains.length === 0 && <span className="px-1 text-xs text-muted">{placeholder || 'Any domain'}</span>}
      </div>
      {adding && <div className="flex gap-2">
        <input autoFocus aria-label="New domain" value={draft} placeholder="team.example.com" autoCapitalize="none" spellCheck={false}
          onChange={(e) => setDraft(e.target.value)} onKeyDown={(e) => { if (e.key === 'Enter') { e.preventDefault(); add() } }}
          className="min-w-0 flex-1 rounded-md border border-line bg-surface px-3 py-2 font-mono text-sm focus:outline-2 focus:outline-accent-strong" />
        <Button kind="quiet" onClick={add} disabled={!draft.trim()}>Add</Button>
      </div>}
      <p className="text-xs text-muted">{domains.length > 0 ? 'Apps can use any hostname under these domains.' : placeholder ? 'Your space name sets the default domain.' : 'Apps will need a full URL. Add a domain to limit their addresses.'}</p>
    </section>
  )
}

export function SpaceConfigEditor({ settings, onChange, creating = false, domainPlaceholder }: {
  settings: SpaceConfig; onChange: (settings: SpaceConfig) => void; creating?: boolean; domainPlaceholder?: string
}) {
  const memberCount = [settings.admins, settings.developers].reduce((n, members) =>
    n + (members.users?.length ?? 0) + (members.groups?.length ?? 0) + (members.roles?.length ?? 0), 0)
  const members = <MembershipEditor admins={settings.admins} developers={settings.developers}
    onChange={(admins, developers) => onChange({ ...settings, admins, developers })} />
  return (
    <div className="space-y-5">
      <DomainSettings domains={settings.domains ?? []} placeholder={domainPlaceholder} onChange={(domains) => onChange({ ...settings, domains })} />
      <details className="rounded-lg border border-line p-3">
        <summary className="cursor-pointer text-sm font-medium">Service offerings</summary>
        <div className="mt-3"><OfferingSettings offerings={settings.offerings} onChange={(offerings) => onChange({ ...settings, offerings })} /></div>
      </details>
      {creating ? <details className="group rounded-lg border border-line">
        <summary className="flex cursor-pointer list-none items-center gap-2.5 px-3 py-3 text-sm [&::-webkit-details-marker]:hidden">
          <Users className="size-4 text-muted" aria-hidden />
          <span className="flex-1 font-medium">Space access</span>
          <span className="text-xs text-muted">{memberCount ? `${memberCount} ${memberCount === 1 ? 'member' : 'members'}` : 'Platform admins only'}</span>
          <ChevronRight className="size-3.5 text-muted transition-transform group-open:rotate-90" aria-hidden />
        </summary>
        <div className="space-y-3 border-t border-line p-3">
          <p className="text-xs text-muted">Add people or teams now, or later in space settings.</p>
          {members}
        </div>
      </details> : <section aria-label="Space access" className="space-y-3">
        <h3 className="flex items-center gap-2 text-sm font-medium"><Users className="size-4 text-muted" aria-hidden /> Space access</h3>
        {members}
      </section>}
    </div>
  )
}

export function SpaceSettings({ space, onClose, onSaved }: {
  space: string; onClose: () => void; onSaved: () => void
}) {
  const queryClient = useQueryClient()
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
      await queryClient.invalidateQueries({ queryKey: ['catalog', space] })
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
        <DialogPanel className="mx-auto w-full max-w-lg rounded-xl border border-line bg-card p-6 shadow-2xl">
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

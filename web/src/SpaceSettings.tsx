import { useEffect, useState, useSyncExternalStore } from 'react'
import { Dialog, DialogBackdrop, DialogPanel, DialogTitle, Tab, TabGroup, TabList, TabPanel, TabPanels } from '@headlessui/react'
import { useQueryClient } from '@tanstack/react-query'
import { Blocks, ChevronRight, Globe2, Plus, Settings, Users, X } from 'lucide-react'
import { api, type SpaceConfig } from './api'
import { MembershipEditor } from './MembershipEditor'
import { OfferingSettings } from './OfferingSettings'
import { Button, ErrorNote, IconButton, Spinner, errText } from './ui'

const sections = [
  { label: 'General', icon: Settings },
  { label: 'Access', icon: Users },
  { label: 'Services', icon: Blocks },
]
const desktopQuery = () => window.matchMedia('(min-width: 640px)')

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
  const desktop = useSyncExternalStore(
    (callback) => {
      const query = desktopQuery()
      query.addEventListener('change', callback)
      return () => query.removeEventListener('change', callback)
    },
    () => desktopQuery().matches,
    () => false,
  )
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
      <div className="fixed inset-0 flex items-center justify-center p-4">
        <DialogPanel className="flex max-h-[calc(100dvh-2rem)] w-full max-w-4xl flex-col overflow-hidden rounded-lg border border-line bg-card shadow-2xl">
          <div className="flex shrink-0 items-center justify-between gap-3 border-b border-line px-4 py-3">
            <DialogTitle className="flex min-w-0 items-center gap-2 font-mono text-[11px] font-semibold uppercase tracking-[0.14em] text-muted">
              <Settings className="size-3.5 shrink-0" aria-hidden />
              <span className="truncate" title={space}>{space} · Settings</span>
            </DialogTitle>
            <IconButton icon={X} label="Close space settings" onClick={onClose} disabled={busy} />
          </div>
          {error && <div className="px-4 pt-3"><ErrorNote message={error} /></div>}
          <fieldset disabled={busy} className="min-h-0 min-w-0 flex-1">
            {settings ? <TabGroup vertical={desktop} className="flex h-full min-h-0 flex-col sm:flex-row">
              <TabList aria-label="Space settings sections" className="flex shrink-0 gap-1 border-b border-line bg-surface/50 p-2 sm:w-40 sm:flex-col sm:border-b-0 sm:border-r">
                {sections.map(({ label, icon: Icon }) => <Tab key={label} disabled={busy}
                  className="flex flex-1 items-center justify-center gap-2 rounded-md px-3 py-2 text-xs text-muted transition-colors hover:bg-raised hover:text-ink focus-visible:outline-2 focus-visible:outline-accent-strong data-selected:bg-accent-soft data-selected:font-medium data-selected:text-ink sm:flex-none sm:justify-start">
                  <Icon className="size-4 shrink-0" aria-hidden />{label}
                </Tab>)}
              </TabList>
              <TabPanels className="max-h-[min(32rem,calc(100dvh-11rem))] min-w-0 flex-1 overflow-y-auto p-4 sm:min-h-64 sm:p-5">
                <TabPanel unmount={false} className="space-y-5 focus:outline-none">
                  <DomainSettings domains={settings.domains ?? []} onChange={(domains) => setSettings({ ...settings, domains })} />
                  {!!settings.domains?.length && <div className="border-t border-line pt-4">
                    <p className="text-xs text-muted">Example app address</p>
                    <p className="mt-1 break-all font-mono text-xs text-ink-soft">https://my-app.{settings.domains[0]}</p>
                  </div>}
                </TabPanel>
                <TabPanel unmount={false} className="space-y-4 focus:outline-none">
                  <div>
                    <h2 className="text-sm font-medium">Members</h2>
                    <p className="mt-1 text-xs text-muted">Give people and teams a role in this space.</p>
                  </div>
                  <MembershipEditor admins={settings.admins} developers={settings.developers}
                    onChange={(admins, developers) => setSettings({ ...settings, admins, developers })} />
                </TabPanel>
                <TabPanel unmount={false} className="focus:outline-none">
                  <OfferingSettings offerings={settings.offerings} onChange={(offerings) => setSettings({ ...settings, offerings })} />
                </TabPanel>
              </TabPanels>
            </TabGroup> : <div className="p-6">{!error && <Spinner label="Loading space settings…" />}</div>}
          </fieldset>
          <div className="flex shrink-0 justify-end gap-2 border-t border-line px-4 py-3">
            <Button kind="quiet" onClick={onClose} disabled={busy}>Cancel</Button>
            <Button onClick={save} disabled={!settings || busy}>{busy ? 'Saving…' : 'Save settings'}</Button>
          </div>
        </DialogPanel>
      </div>
    </Dialog>
  )
}

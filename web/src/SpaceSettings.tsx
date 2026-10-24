import { useEffect, useState } from 'react'
import { Tab, TabGroup, TabList, TabPanel, TabPanels } from '@headlessui/react'
import { useQueryClient } from '@tanstack/react-query'
import { Blocks, Globe2, Plus, Settings, Users, X } from 'lucide-react'
import { api, type SpaceConfig } from './api'
import { Modal, ModalBody, ModalFooter, ModalHeader, ModalNotice } from './dialog'
import { MembershipEditor } from './MembershipEditor'
import { OfferingSettings } from './OfferingSettings'
import { Button, Field, IconButton, Input, List, ListRow, SectionHeader, Spinner, ToggleRow, errText, useMediaQuery } from './ui'

const sections = [
  { label: 'General', icon: Settings },
  { label: 'Access', icon: Users },
  { label: 'Services', icon: Blocks },
]

export function DomainSettings({ domains, onChange, placeholder }: {
  domains: string[]; onChange: (domains: string[]) => void; placeholder?: string
}) {
  const [draft, setDraft] = useState('')
  function add() {
    const domain = draft.trim().toLowerCase()
    if (!domain) return
    onChange([...new Set([...domains, domain])])
    setDraft('')
  }
  return (
    <section aria-label="Domains" className="space-y-3">
      <SectionHeader
        title="Domains"
        description={domains.length > 0
          ? 'Apps can use any hostname under these domains.'
          : placeholder
            ? 'Your space name sets the default domain. Add your own to use it instead.'
            : 'Apps will need a full URL. Add a domain to limit their addresses.'}
      />
      <List>
        {domains.length === 0 && placeholder && (
          <ListRow><Globe2 className="size-4 shrink-0 text-muted" aria-hidden /><span className="font-mono text-sm text-muted">{placeholder}</span></ListRow>
        )}
        {domains.map((domain) => (
          <ListRow key={domain} actions={
            <IconButton icon={X} size="sm" kind="danger" label={`Remove domain ${domain}`} onClick={() => onChange(domains.filter((d) => d !== domain))} />
          }>
            <Globe2 className="size-4 shrink-0 text-muted" aria-hidden />
            <span className="min-w-0 flex-1 truncate font-mono text-sm" title={domain}>{domain}</span>
            <span className="hidden truncate font-mono text-xs text-muted sm:inline">https://my-app.{domain}</span>
          </ListRow>
        ))}
        <ListRow className="bg-surface/40" actions={<IconButton icon={Plus} size="sm" label="Add domain" onClick={add} disabled={!draft.trim()} />}>
          <Plus className="size-4 shrink-0 text-muted" aria-hidden />
          <Input bare aria-label="New domain" value={draft} placeholder="Add a domain — team.example.com" autoCapitalize="none" spellCheck={false}
            onChange={(e) => setDraft(e.target.value)}
            onKeyDown={(e) => { if (e.key === 'Enter') { e.preventDefault(); add() } }}
            className="flex-1" />
        </ListRow>
      </List>
    </section>
  )
}

/**
 * The space form, shared by Create space and Space settings so the two
 * dialogs are one design: vertical tabs on the left, one subject per tab.
 * When `name` is given the form is creating a space and asks for its name
 * at the top of General.
 */
export function SpaceForm({ settings, onChange, name, disabled = false }: {
  settings: SpaceConfig
  onChange: (settings: SpaceConfig) => void
  name?: { value: string; onChange: (name: string) => void; domainPlaceholder?: string }
  disabled?: boolean
}) {
  const desktop = useMediaQuery('(min-width: 640px)')
  return (
    <fieldset disabled={disabled} className="flex min-h-0 flex-1 flex-col">
      <TabGroup vertical={desktop} className="flex min-h-0 flex-1 flex-col sm:flex-row">
        <TabList aria-label="Space settings sections" className="flex shrink-0 gap-0.5 border-b border-line px-3 py-2 sm:w-40 sm:flex-col sm:border-b-0 sm:border-r sm:py-4">
          {sections.map(({ label, icon: Icon }) => <Tab key={label}
            className="group flex h-8 flex-1 items-center justify-center gap-2 rounded-md px-2.5 text-sm text-muted transition-colors hover:text-ink focus-visible:outline-2 focus-visible:outline-accent-strong data-selected:bg-raised data-selected:font-medium data-selected:text-ink sm:flex-none sm:justify-start">
            <Icon className="size-4 shrink-0 text-muted group-data-selected:text-ink" aria-hidden />{label}
          </Tab>)}
        </TabList>
        <TabPanels className="min-h-0 min-w-0 flex-1 overflow-y-auto px-5 py-5">
          <TabPanel unmount={false} className="space-y-6 focus:outline-none">
            {name && <Field label="Space name" hint="Lowercase letters, digits and dashes, up to 40 characters.">
              <Input
                autoFocus
                value={name.value}
                onChange={(e) => name.onChange(e.target.value)}
                placeholder="my-project"
                pattern="[a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?"
                maxLength={40}
                required
                title="Use lowercase letters, digits and dashes, starting and ending with a letter or digit."
                autoCapitalize="none"
                spellCheck={false}
              />
            </Field>}
            <ToggleRow label="Allow public apps"
              description="Off by default. Turning this off switches existing public apps to login protection and prevents public deployments."
              checked={settings.allowPublic ?? false} onChange={(allowPublic) => onChange({ ...settings, allowPublic })} />
            <DomainSettings domains={settings.domains ?? []} placeholder={name?.domainPlaceholder} onChange={(domains) => onChange({ ...settings, domains })} />
          </TabPanel>
          <TabPanel unmount={false} className="space-y-3 focus:outline-none">
            <SectionHeader title="Members"
              description={name ? 'Add people or teams now, or later in space settings.' : 'Give people and teams a role in this space.'} />
            <MembershipEditor admins={settings.admins} developers={settings.developers} viewers={settings.viewers ?? {}}
              onChange={(admins, developers, viewers) => onChange({ ...settings, admins, developers, viewers })} />
          </TabPanel>
          <TabPanel unmount={false} className="focus:outline-none">
            <OfferingSettings offerings={settings.offerings} onChange={(offerings) => onChange({ ...settings, offerings })} />
          </TabPanel>
        </TabPanels>
      </TabGroup>
    </fieldset>
  )
}

export function SpaceSettings({ space, onClose, onSaved }: {
  space: string; onClose: () => void; onSaved: () => void
}) {
  const queryClient = useQueryClient()
  const [settings, setSettings] = useState<SpaceConfig | null>(null)
  const [saved, setSaved] = useState<string>('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  // Save is offered only once something differs from what the server holds.
  const dirty = settings !== null && JSON.stringify(settings) !== saved
  useEffect(() => {
    let active = true
    api.spaceConfig(space).then((value) => { if (active) { setSettings(value); setSaved(JSON.stringify(value)) } })
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

  // A fixed height: switching between General, Access and Services must not
  // resize the frame — the content scrolls inside its panel instead.
  return (
    <Modal onClose={() => !busy && onClose()} size="lg" height="medium">
      <ModalHeader eyebrow="Space settings" title={<span className="font-mono">{space}</span>}
        onClose={onClose} closeLabel="Close space settings" closeDisabled={busy} />
      <ModalNotice message={error} />
      <ModalBody padded={false}>
        {settings
          ? <SpaceForm settings={settings} onChange={setSettings} disabled={busy} />
          : <div className="flex flex-1 items-center justify-center">{!error && <Spinner label="Loading space settings…" />}</div>}
      </ModalBody>
      <ModalFooter>
        <Button kind="quiet" onClick={onClose} disabled={busy}>Cancel</Button>
        <Button onClick={save} disabled={!dirty || busy}>{busy ? 'Saving…' : 'Save settings'}</Button>
      </ModalFooter>
    </Modal>
  )
}

import { useCallback, useEffect, useState } from 'react'
import { Globe, Lock, Network, type LucideIcon } from 'lucide-react'
import { api, type AccessConfig, type AppMode, type Exposure, type NetworkInfo } from './api'
import { HostEditor } from './HostEditor'
import { MembershipEditor } from './MembershipEditor'
import { APP_MODES, Button, ChoiceCards, Eyebrow, ErrorNote, errText, loginHint, Section, SectionHeader, Spinner, SuccessNote, ToggleRow } from './ui'

/**
 * How the app is reached, changed on the spot — no deploy.
 *
 * The donkeyfile stays the source of truth where it speaks: a choice made
 * here holds until donkey.yaml itself sets mode/public/internal/host/access/cors,
 * which always wins on the next deploy.
 */
export function NetworkCard({
  space,
  app,
  onChanged,
}: {
  space: string
  app: string
  onChanged: () => void
}) {
  const [net, setNet] = useState<NetworkInfo | null>(null)
  const [mode, setMode] = useState<AppMode>('web')
  const [exposure, setExposure] = useState<Exposure>('protected')
  const [host, setHost] = useState('')
  const [cors, setCors] = useState<'' | 'allowAll'>('')
  const [busy, setBusy] = useState(false)
  const [note, setNote] = useState('')
  const [error, setError] = useState('')

  const load = useCallback(() => {
    api
      .network(space, app)
      .then((info) => {
        setNet(info)
        setMode(info.mode)
        setExposure(info.exposure)
        setHost(info.fqdn ?? '')
        setCors(info.cors)
      })
      .catch((err) => setError(errText(err, 'Loading failed.')))
  }, [space, app])

  useEffect(() => {
    load()
  }, [load])

  const dirty =
    net !== null &&
    (exposure !== net.exposure ||
      mode !== net.mode ||
      (host.trim() || '') !== (net.fqdn ?? '') ||
      cors !== net.cors)

  async function apply() {
    setBusy(true)
    setError('')
    setNote('')
    try {
      const res = await api.setNetwork(space, app, {
        mode,
        exposure,
        host: exposure === 'internal' ? '' : host.trim(),
        cors,
      })
      setNet(res.network)
      setMode(res.network.mode)
      setExposure(res.network.exposure)
      setHost(res.network.fqdn ?? '')
      setCors(res.network.cors)
      setNote(res.note || 'Applied — the change is live.')
      onChanged()
    } catch (err) {
      setError(errText(err, 'Applying did not work.'))
    } finally {
      setBusy(false)
    }
  }

  const options: { value: Exposure; icon: LucideIcon; label: string; hint: string }[] = [
    {
      value: 'protected',
      icon: Lock,
      label: 'Behind the login',
      hint: `${loginHint(mode)} Narrow it further below.`,
    },
    {
      value: 'public',
      icon: Globe,
      label: 'Public',
      hint: 'Anyone with the address can reach it. What webhook senders and open APIs need.',
    },
    {
      value: 'internal',
      icon: Network,
      label: 'Internal',
      hint: `No address, no certificate — only the apps of ${space} reach it, as http://${app}.`,
    },
  ]

  return (
    <>
      <Section className="space-y-5">
        <SectionHeader
          title="Ingress"
          description="Applies immediately, without a deploy. An explicit setting in donkey.yaml wins again at the next deploy."
        />
        {net === null && !error && <Spinner label="Loading…" />}
        <ErrorNote message={error} />
        {net !== null && (
          <>
            <div className="space-y-1.5">
              <Eyebrow>Kind of app</Eyebrow>
              <ChoiceCards label="Kind of app" value={mode} onChange={setMode} options={APP_MODES} />
            </div>
            <div className="space-y-1.5">
              <Eyebrow>Who can reach it</Eyebrow>
              <ChoiceCards label="Exposure" value={exposure} onChange={setExposure} options={options} columns="sm:grid-cols-3" />
            </div>

            {exposure !== 'internal' && (
              <HostEditor value={host} domains={net.domains ?? []} onChange={setHost} />
            )}

            {(net.routes ?? []).length > 0 && (
              <div className="space-y-1.5">
                <Eyebrow>Shared addresses</Eyebrow>
                <ul className="space-y-1">
                  {(net.routes ?? []).map((r) => (
                    <li key={r.host + r.path} className="font-mono text-xs text-ink-soft">
                      https://{r.host}
                      <span className="text-accent">{r.path}</span>
                    </li>
                  ))}
                </ul>
                <p className="text-xs text-muted">
                  From <code className="font-mono">routes:</code> in donkey.yaml — they follow the exposure above.
                </p>
              </div>
            )}

            {exposure !== 'internal' && (
              <ToggleRow
                label="Allow calls from any browser origin"
                description="The gateway answers CORS preflights and adds Access-Control-Allow-Origin: *."
                checked={cors === 'allowAll'}
                onChange={(on) => setCors(on ? 'allowAll' : '')}
              />
            )}

            <div className="flex items-center justify-end gap-3">
              <SuccessNote message={note} />
              <Button onClick={apply} disabled={!dirty || busy || (exposure !== 'internal' && !host.trim())}>
                {busy ? 'Applying…' : 'Apply'}
              </Button>
            </div>
          </>
        )}
      </Section>

      {net && exposure === 'protected' && (
        <AccessCard space={space} app={app} initial={net.access ?? null} />
      )}
    </>
  )
}

/**
 * Who may open the app, once they are signed in. An empty list means
 * everyone the identity provider knows — donkey's default promise. The same
 * member list as a space's access, without roles.
 */
function AccessCard({ space, app, initial }: { space: string; app: string; initial: AccessConfig | null }) {
  const [access, setAccess] = useState<AccessConfig>(initial ?? {})
  const [dirty, setDirty] = useState(false)
  const [busy, setBusy] = useState(false)
  const [note, setNote] = useState('')
  const [error, setError] = useState('')

  useEffect(() => {
    setAccess(initial ?? {})
    setDirty(false)
  }, [initial])

  const count = (access.users?.length ?? 0) + (access.groups?.length ?? 0) + (access.roles?.length ?? 0)

  async function save() {
    setBusy(true)
    setError('')
    setNote('')
    try {
      await api.setAccess(space, app, { users: access.users ?? [], groups: access.groups ?? [], roles: access.roles ?? [] })
      setDirty(false)
      setNote(
        count === 0
          ? 'Saved — every signed-in user may open the app again.'
          : 'Saved — the gateway enforces the new list within seconds.',
      )
    } catch (err) {
      setError(errText(err, 'Saving did not work.'))
    } finally {
      setBusy(false)
    }
  }

  return (
    <Section className="space-y-3">
      <SectionHeader
        title="Access"
        description={
          count > 0
            ? 'Only the people below may open the app. Users match by email, groups and roles by the claims of the sign-in.'
            : 'Everyone the identity provider signs in may open the app. Add users, groups or roles to narrow it.'
        }
      />
      <MembershipEditor
        admins={access}
        emptyText="Everyone who signs in."
        onChange={(next) => {
          setAccess(next)
          setDirty(true)
        }}
      />
      <ErrorNote message={error} />
      <div className="flex items-center justify-end gap-3">
        <SuccessNote message={note} />
        <Button onClick={save} disabled={!dirty || busy}>
          {busy ? 'Saving…' : 'Save access'}
        </Button>
      </div>
    </Section>
  )
}

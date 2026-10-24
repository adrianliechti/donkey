import { useCallback, useEffect, useState } from 'react'
import { useQueryClient } from '@tanstack/react-query'
import { Globe, Lock, Network, type LucideIcon } from 'lucide-react'
import { api, type AppMode, type Exposure, type NetworkInfo } from './api'
import { HostEditor } from './HostEditor'
import { networkQuery } from './queries'
import { Button, ChoiceCards, ErrorNote, Eyebrow, Section, SectionHeader, Spinner, SuccessNote, ToggleRow } from './ui'
import { APP_MODES, loginHint } from './vocabulary'
import { errText } from './format'

/**
 * How the app is reached, changed on the spot — no deploy.
 *
 * The donkeyfile stays the source of truth where it speaks: a choice made
 * here holds until donkey.yaml itself sets mode/public/internal/host/cors/analytics,
 * which always wins on the next deploy.
 */
export function NetworkCard({ space, app, onChanged }: { space: string; app: string; onChanged: () => void }) {
  const [net, setNet] = useState<NetworkInfo | null>(null)
  const [mode, setMode] = useState<AppMode>('web')
  const [exposure, setExposure] = useState<Exposure>('protected')
  const [host, setHost] = useState('')
  const [cors, setCors] = useState<'' | 'allowAll'>('')
  const [analytics, setAnalytics] = useState(true)
  const [busy, setBusy] = useState(false)
  const [note, setNote] = useState('')
  const [error, setError] = useState('')
  const queryClient = useQueryClient()

  const load = useCallback(() => {
    api
      .network(space, app)
      .then((info) => {
        setNet(info)
        setMode(info.mode)
        setExposure(info.exposure)
        setHost(info.fqdn ?? '')
        setCors(info.cors)
        setAnalytics(info.analytics !== false)
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
      cors !== net.cors ||
      analytics !== (net.analytics !== false))

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
        analytics,
      })
      setNet(res.network)
      setMode(res.network.mode)
      setExposure(res.network.exposure)
      setHost(res.network.fqdn ?? '')
      setCors(res.network.cors)
      setAnalytics(res.network.analytics !== false)
      // The Analytics tab reads the same settings to say whether it counts.
      queryClient.setQueryData(networkQuery(space, app).queryKey, res.network)
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
      hint: `${loginHint(mode)} Access is managed in space settings.`,
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
            <ChoiceCards
              label="Exposure"
              value={exposure}
              onChange={setExposure}
              options={options.filter((option) => option.value !== 'public' || net.allowPublic)}
              columns={net.allowPublic ? 'sm:grid-cols-3' : 'sm:grid-cols-2'}
            />
          </div>

          {exposure !== 'internal' && <HostEditor value={host} domains={net.domains ?? []} onChange={setHost} />}

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

          {exposure !== 'internal' && mode === 'web' && (
            <ToggleRow
              label="Web analytics"
              description="donkey-gateway adds a cookieless analytics script to this app's HTML pages — page views, visits and custom events show up under Analytics."
              checked={analytics}
              onChange={setAnalytics}
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
  )
}

import { useCallback, useEffect, useState } from 'react'
import { Globe, KeyRound, Lock, Network, Plus, X } from 'lucide-react'
import { api, type AccessConfig, type AppMode, type Exposure, type NetworkInfo } from './api'
import { Button, ErrorNote, errText, SectionTitle, Spinner } from './ui'

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
        setHost(info.host ?? '')
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
      (host.trim() || '') !== (net.host ?? '') ||
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
      setHost(res.network.host ?? '')
      setCors(res.network.cors)
      setNote(res.note || 'Applied — the change is live.')
      onChanged()
    } catch (err) {
      setError(errText(err, 'Applying did not work.'))
    } finally {
      setBusy(false)
    }
  }

  const options: { id: Exposure; icon: React.ReactNode; label: string; hint: string }[] = [
    {
      id: 'protected',
      icon: <Lock className="size-4" aria-hidden />,
      label: 'Login-protected',
      hint: 'Visitors sign in through the identity provider. Narrow it further below.',
    },
    {
      id: 'public',
      icon: <Globe className="size-4" aria-hidden />,
      label: 'Public',
      hint: 'Anyone with the address can open it. For sites and machine-facing APIs.',
    },
    {
      id: 'internal',
      icon: <Network className="size-4" aria-hidden />,
      label: 'Internal',
      hint: `No address, no certificate — only the apps of ${space} reach it, as http://${app}.`,
    },
  ]

  return (
    <>
      <section>
        <SectionTitle>Ingress</SectionTitle>
        {net === null && !error && <Spinner label="Loading…" />}
        <ErrorNote message={error} />
        {net !== null && (
          <div className="mt-3 space-y-4">
            <div className="grid gap-2 sm:grid-cols-2" role="radiogroup" aria-label="Application mode">
              <button
                role="radio"
                aria-checked={mode === 'web'}
                onClick={() => setMode('web')}
                className={`rounded-lg border p-3 text-left ${mode === 'web' ? 'border-accent bg-accent-soft' : 'border-line hover:border-line-strong'}`}
              >
                <span className="flex items-center gap-1.5 text-sm font-medium">
                  <Globe className="size-4" aria-hidden /> Web / HTTP
                </span>
                <span className="mt-1 block text-xs text-muted">Websites, APIs and webhooks.</span>
              </button>
              <button
                role="radio"
                aria-checked={mode === 'api'}
                onClick={() => setMode('api')}
                className={`rounded-lg border p-3 text-left ${mode === 'api' ? 'border-accent bg-accent-soft' : 'border-line hover:border-line-strong'}`}
              >
                <span className="flex items-center gap-1.5 text-sm font-medium">
                  <KeyRound className="size-4" aria-hidden /> API
                </span>
                <span className="mt-1 block text-xs text-muted">
                  For MCP and ordinary APIs. Protected adds OAuth; public and internal have no login.
                </span>
              </button>
            </div>

            <div className="grid gap-2 sm:grid-cols-3" role="radiogroup" aria-label="Exposure">
              {options.map((o) => (
                <button
                  key={o.id}
                  role="radio"
                  aria-checked={exposure === o.id}
                  onClick={() => setExposure(o.id)}
                  className={`rounded-lg border p-3 text-left transition-colors focus-visible:outline-2 focus-visible:outline-accent-strong ${
                    exposure === o.id
                      ? 'border-accent bg-accent-soft'
                      : 'border-line hover:border-line-strong'
                  }`}
                >
                  <span className={`flex items-center gap-1.5 text-sm font-medium ${exposure === o.id ? 'text-accent' : ''}`}>
                    {o.icon}
                    {o.label}
                  </span>
                  <span className="mt-1 block text-xs leading-snug text-muted">{o.hint}</span>
                </button>
              ))}
            </div>

            {exposure !== 'internal' && (
              <label className="grid grid-cols-[6.5rem_minmax(0,1fr)] border-y border-line/70">
                <span className="px-3 py-2.5 text-xs font-medium text-muted">Address</span>
                <span className="min-w-0 border-l border-line/60">
                  <input
                    value={host}
                    onChange={(e) => setHost(e.target.value)}
                    placeholder={net.defaultHost}
                    className="block w-full bg-transparent px-3 pt-2.5 pb-1 font-mono text-sm placeholder:text-muted/60 focus:bg-raised/30 focus:outline-none focus:ring-1 focus:ring-inset focus:ring-accent-strong"
                  />
                  <span className="block px-3 pb-2.5 text-xs leading-relaxed text-muted">
                    Leave empty for <span className="font-mono">{net.defaultHost}</span>. A plain name
                    sits under the space, <span className="font-mono">@</span> takes the space's own
                    address; a full hostname needs its DNS pointed at the gateway.
                  </span>
                </span>
              </label>
            )}

            {(net.routes ?? []).length > 0 && (
              <div>
                <span className="text-sm font-medium">Shared addresses</span>
                <ul className="mt-1 space-y-1">
                  {(net.routes ?? []).map((r) => (
                    <li key={r.host + r.path} className="font-mono text-xs text-ink-soft">
                      https://{r.host}
                      <span className="text-accent">{r.path}</span>
                    </li>
                  ))}
                </ul>
                <p className="mt-1 text-xs text-muted">
                  From <code className="font-mono">routes:</code> in donkey.yaml — they follow the
                  exposure above.
                </p>
              </div>
            )}

            {exposure !== 'internal' && (
              <label className="flex cursor-pointer items-start gap-2.5 rounded-md border border-line px-3 py-2.5">
                <input
                  type="checkbox"
                  checked={cors === 'allowAll'}
                  onChange={(event) => setCors(event.target.checked ? 'allowAll' : '')}
                  className="mt-0.5 size-4 shrink-0 accent-accent-strong"
                />
                <span>
                  <span className="block text-sm font-medium">Allow calls from any browser origin</span>
                  <span className="block text-xs text-muted">
                    The gateway answers CORS preflights and adds Access-Control-Allow-Origin: *.
                  </span>
                </span>
              </label>
            )}

            <div className="flex flex-wrap items-center justify-between gap-2 border-t border-line pt-3">
              <p className="text-xs text-muted">
                Applies immediately, without a deploy. An explicit setting in donkey.yaml wins again
                at the next deploy.
              </p>
              <Button onClick={apply} disabled={!dirty || busy}>
                {busy ? 'Applying…' : 'Apply'}
              </Button>
            </div>
            {note && (
              <p role="status" className="text-sm text-success">
                {note}
              </p>
            )}
          </div>
        )}
      </section>

      {net && exposure === 'protected' && (
        <AccessCard space={space} app={app} initial={net.access ?? null} />
      )}
    </>
  )
}

/**
 * Who may open the app, once they are signed in. Empty lists mean everyone
 * the identity provider knows — donkey's default promise.
 */
function AccessCard({ space, app, initial }: { space: string; app: string; initial: AccessConfig | null }) {
  const [users, setUsers] = useState<string[]>(initial?.users ?? [])
  const [groups, setGroups] = useState<string[]>(initial?.groups ?? [])
  const [roles, setRoles] = useState<string[]>(initial?.roles ?? [])
  const [dirty, setDirty] = useState(false)
  const [busy, setBusy] = useState(false)
  const [note, setNote] = useState('')
  const [error, setError] = useState('')

  useEffect(() => {
    setUsers(initial?.users ?? [])
    setGroups(initial?.groups ?? [])
    setRoles(initial?.roles ?? [])
    setDirty(false)
  }, [initial])

  async function save() {
    setBusy(true)
    setError('')
    setNote('')
    try {
      await api.setAccess(space, app, { users, groups, roles })
      setDirty(false)
      setNote(
        users.length + groups.length + roles.length === 0
          ? 'Saved — every signed-in user may open the app again.'
          : 'Saved — the gateway enforces the new list within seconds.',
      )
    } catch (err) {
      setError(errText(err, 'Saving did not work.'))
    } finally {
      setBusy(false)
    }
  }

  const restricted = users.length + groups.length + roles.length > 0

  return (
    <section className="border-t border-line pt-5">
      <SectionTitle>Access</SectionTitle>
      <p className="mt-2 text-sm text-ink-soft">
        {restricted
          ? 'Only the people below may open the app. A visitor passes by matching any list.'
          : 'Everyone the identity provider signs in may open the app. Add people, groups or roles to narrow it.'}
      </p>
      <div className="mt-3 space-y-3">
        <ListEditor
          label="Members"
          placeholder="ada@example.com"
          hint="Email addresses, as the identity provider reports them."
          values={users}
          onChange={(v) => {
            setUsers(v)
            setDirty(true)
          }}
        />
        <ListEditor
          label="Groups"
          placeholder="engineering"
          hint="Matched against the groups claim of the sign-in."
          values={groups}
          onChange={(v) => {
            setGroups(v)
            setDirty(true)
          }}
        />
        <ListEditor
          label="Roles"
          placeholder="admin"
          hint="Matched against the roles claim (Entra app roles, Keycloak realm roles)."
          values={roles}
          onChange={(v) => {
            setRoles(v)
            setDirty(true)
          }}
        />
      </div>
      <div className="mt-3 flex items-center justify-end gap-2 border-t border-line pt-3">
        <Button onClick={save} disabled={!dirty || busy}>
          {busy ? 'Saving…' : 'Save access'}
        </Button>
      </div>
      {note && (
        <p role="status" className="mt-2 text-sm text-success">
          {note}
        </p>
      )}
      <ErrorNote message={error} />
    </section>
  )
}

function ListEditor({
  label,
  placeholder,
  hint,
  values,
  onChange,
}: {
  label: string
  placeholder: string
  hint: string
  values: string[]
  onChange: (values: string[]) => void
}) {
  const [draft, setDraft] = useState('')

  function add(e?: React.FormEvent) {
    e?.preventDefault()
    const value = draft.trim()
    if (!value || values.includes(value)) {
      setDraft('')
      return
    }
    onChange([...values, value])
    setDraft('')
  }

  return (
    <div>
      <span className="text-sm font-medium">{label}</span>
      <div className="mt-1 flex flex-wrap items-center gap-1.5">
        {values.map((v) => (
          <span
            key={v}
            className="inline-flex items-center gap-1 rounded-md bg-raised px-2 py-1 font-mono text-xs"
          >
            {v}
            <button
              type="button"
              aria-label={`Remove ${v}`}
              onClick={() => onChange(values.filter((x) => x !== v))}
              className="rounded p-0.5 text-muted hover:bg-danger/20 hover:text-danger"
            >
              <X className="size-3" aria-hidden />
            </button>
          </span>
        ))}
        <form onSubmit={add} className="flex items-center gap-1">
          <input
            value={draft}
            onChange={(e) => setDraft(e.target.value)}
            placeholder={placeholder}
            aria-label={`Add to ${label}`}
            className="w-44 rounded-md border border-line bg-surface px-2 py-1 font-mono text-xs placeholder:text-muted/60 focus:outline-2 focus:outline-accent-strong"
          />
          <button
            type="submit"
            aria-label={`Add to ${label}`}
            disabled={draft.trim() === ''}
            className="rounded-md border border-line p-1 text-muted hover:bg-raised hover:text-ink disabled:opacity-40"
          >
            <Plus className="size-3.5" aria-hidden />
          </button>
        </form>
      </div>
      <p className="mt-1 text-xs text-muted">{hint}</p>
    </div>
  )
}

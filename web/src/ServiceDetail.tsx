import { lazy, Suspense, useState, type ComponentType, type ReactNode } from 'react'
import { Link } from '@tanstack/react-router'
import { Eye, EyeOff, Link2, Loader2, Trash2, Unlink2 } from 'lucide-react'
import type { SpaceAddonEntry } from './api'
import { AppIcon } from './AppIcon'
import { consoleModules, consoleTabs, serviceTabs, type ConsoleKind, type ConsoleProps } from './consoles'
import { RemoveServiceDialog } from './SpaceActions'
import { mutationError, useConnectService, useFleet, useServiceBinding, useSpaceServices } from './queries'
import { serviceUI } from './serviceProtocol'
import { Sheet } from './Sheet'
import {
  CopyButton,
  ErrorNote,
  IconButton,
  List,
  ListRow,
  Section,
  SectionHeader,
  Spinner,
  StateChip,
  TabLabel,
} from './ui'
import { healthOf } from './vocabulary'

// One lazy component per console, made once: a remount must not refetch it.
const consoles = {} as Record<ConsoleKind, ComponentType<ConsoleProps>>
for (const kind of Object.keys(consoleModules) as ConsoleKind[]) {
  consoles[kind] = lazy(() => consoleModules[kind]().then((m) => ({ default: m.Console })))
}

/**
 * A service is a page of its own, like an app: its console in the first
 * tabs — the database browser, the keys, the bucket, the chat — then how
 * apps reach it and how to remove it. The tab row sticks under the
 * breadcrumb, which names the service and switches to the next one.
 */
export function ServiceDetail({
  space,
  service,
  tab,
  onTab,
  onRemoved,
}: {
  space: string
  service: string
  tab?: string
  onTab: (tab: string) => void
  onRemoved: () => void
}) {
  const { data: services, error } = useSpaceServices(space)
  const entry = services?.find((s) => s.name === service)
  const tabs = serviceTabs(entry?.protocol)
  const shown = tabs.find((t) => t.id === tab)?.id ?? tabs[0].id
  const kind = serviceUI(entry?.protocol).console
  const Console = kind ? consoles[kind] : undefined
  const ownTab = kind ? consoleTabs[kind].some((t) => t.id === shown) : false

  return (
    // A console is a workbench: the page stands still and its panes scroll
    // (see Sheet). Connection and Settings are documents.
    <Sheet
      fill={!!entry && ownTab && !!Console}
      tabs={
        entry && (
          <div className="flex items-center justify-between gap-4 px-6">
            <nav
              className="-mb-px -ml-3 flex min-w-0 gap-1 self-stretch overflow-x-auto overflow-y-hidden pt-1 scrollbar-none"
              role="tablist"
            >
              {tabs.map((t) => {
                const Icon = t.icon
                return (
                  <button
                    key={t.id}
                    role="tab"
                    aria-selected={shown === t.id}
                    onClick={() => onTab(t.id)}
                    className={`flex h-11 shrink-0 items-center gap-1.5 border-b-2 px-3 text-sm transition-colors focus-visible:outline-2 focus-visible:outline-accent-strong ${
                      shown === t.id
                        ? 'border-accent font-medium text-ink'
                        : 'border-transparent text-muted hover:border-line hover:text-ink'
                    }`}
                  >
                    <Icon className="size-4" aria-hidden />
                    <TabLabel>{t.label}</TabLabel>
                  </button>
                )
              })}
            </nav>
            <div className="flex shrink-0 items-center gap-2 py-2">
              {entry.state && entry.state !== 'ready' && <StateChip state={entry.state} />}
              {entry.type && <span className="font-mono text-xs text-muted">{entry.type}</span>}
            </div>
          </div>
        )
      }
    >
      {!entry ? (
        <div className="space-y-4 px-6 py-6">
          <ErrorNote message={error} />
          {services === null && !error && <Spinner label="Opening the service…" />}
          {services && (
            <p className="text-sm text-muted">
              {space} has no service named <span className="font-mono text-ink">{service}</span>.{' '}
              <Link to="/space/$space" params={{ space }} className="text-accent-strong hover:underline">
                Back to {space}
              </Link>
            </p>
          )}
        </div>
      ) : ownTab && Console ? (
        <div className="flex min-h-0 flex-1 flex-col px-6 py-6">
          <Suspense fallback={<Spinner label="Opening the console…" />}>
            <Console space={space} service={service} protocol={entry.protocol} tab={shown} />
          </Suspense>
        </div>
      ) : shown === 'settings' ? (
        <div className="flex w-full flex-col gap-5 px-6 py-6">
          <ServiceSettings space={space} entry={entry} onRemoved={onRemoved} />
        </div>
      ) : (
        <div className="flex w-full flex-col gap-5 px-6 py-6">
          <Connection space={space} entry={entry} />
        </div>
      )}
    </Sheet>
  )
}

// Connection: what an app gets when it connects, which apps have, and the
// way in from a laptop.
function Connection({ space, entry }: { space: string; entry: SpaceAddonEntry }) {
  return (
    <>
      <Variables space={space} entry={entry} />
      <ConnectedApps space={space} entry={entry} />
      <Section className="space-y-3">
        <SectionHeader
          title="From your machine"
          description="A private line through donkey, open while the command runs. Nothing is exposed."
        />
        <Command text={`donkey addon tunnel --space ${space} ${entry.name}`} />
      </Section>
    </>
  )
}

function Variables({ space, entry }: { space: string; entry: SpaceAddonEntry }) {
  const { data, error } = useServiceBinding(space, entry.name)
  const [shown, setShown] = useState<Set<string>>(new Set())
  const toggle = (key: string) =>
    setShown((current) => {
      const next = new Set(current)
      if (next.has(key)) next.delete(key)
      else next.add(key)
      return next
    })
  const keys = data ? Object.keys(data.binding).sort() : entry.keys

  return (
    <Section className="space-y-3">
      <SectionHeader
        title="Variables"
        description="What a connected app finds in its environment. Values are secrets — they show only when asked."
      />
      <ErrorNote message={error} />
      {keys.length === 0 ? (
        <p className="text-sm text-muted">This service brings no variables.</p>
      ) : (
        <List>
          {keys.map((key) => {
            const value = data?.binding[key]
            const open = shown.has(key)
            return (
              <ListRow
                key={key}
                actions={
                  value !== undefined && (
                    <>
                      <IconButton
                        icon={open ? EyeOff : Eye}
                        size="sm"
                        label={open ? `Hide ${key}` : `Show ${key}`}
                        onClick={() => toggle(key)}
                      />
                      <CopyButton text={value} label={`Copy ${key}`} />
                    </>
                  )
                }
              >
                <span className="w-64 shrink-0 truncate font-mono text-sm text-ink">{key}</span>
                <span className="min-w-0 flex-1 truncate font-mono text-xs text-muted">
                  {value === undefined ? '' : open ? value : '••••••••••••'}
                </span>
              </ListRow>
            )
          })}
        </List>
      )}
    </Section>
  )
}

function ConnectedApps({ space, entry }: { space: string; entry: SpaceAddonEntry }) {
  const { data: fleet } = useFleet()
  const connect = useConnectService(space)
  const [pending, setPending] = useState('')
  const unready = Boolean(entry.state && entry.state !== 'ready')
  const apps = (fleet?.apps ?? [])
    .filter((app) => (app.space || 'default') === space)
    .map((app) => ({ app, on: entry.attachedTo.includes(app.name) }))
    .sort((a, b) => Number(b.on) - Number(a.on) || a.app.name.localeCompare(b.app.name))

  const run = (app: string, on: boolean) => {
    setPending(app)
    connect.mutate({ app, service: entry.name, connect: on }, { onSettled: () => setPending('') })
  }

  return (
    <Section className="space-y-3">
      <SectionHeader
        title="Apps"
        description="Connecting gives the app these variables and restarts it. Disconnecting deletes nothing."
      />
      <ErrorNote message={mutationError(connect, 'That did not work.')} />
      {apps.length === 0 ? (
        <p className="text-sm text-muted">{space} has no apps yet.</p>
      ) : (
        <List>
          {apps.map(({ app, on }) => (
            <ListRow
              key={app.name}
              actions={
                pending === app.name ? (
                  <span className="flex size-7 items-center justify-center text-muted">
                    <Loader2 className="size-3.5 animate-spin" aria-hidden />
                  </span>
                ) : on ? (
                  <IconButton
                    icon={Unlink2}
                    size="sm"
                    kind="danger"
                    label={`Disconnect ${app.name}`}
                    disabled={pending !== ''}
                    onClick={() => run(app.name, false)}
                  />
                ) : (
                  <IconButton
                    icon={Link2}
                    size="sm"
                    label={`Connect ${app.name}`}
                    disabled={pending !== '' || unready}
                    onClick={() => run(app.name, true)}
                  />
                )
              }
            >
              <AppIcon health={healthOf(app.healthy, app.ready)} stack={app.stack} />
              <Link
                to="/app/$space/$name/{-$tab}"
                params={{ space, name: app.name, tab: undefined }}
                className={`font-mono text-sm hover:underline ${on ? 'text-ink' : 'text-muted'}`}
              >
                {app.name}
              </Link>
              {!on && <span className="text-xs text-muted">not connected</span>}
            </ListRow>
          ))}
        </List>
      )}
    </Section>
  )
}

function Command({ text }: { text: string }) {
  return (
    <div className="flex items-center gap-2 rounded-lg border border-line bg-well py-1.5 pr-1.5 pl-3">
      <code className="min-w-0 flex-1 truncate font-mono text-xs text-emphasis">{text}</code>
      <CopyButton text={text} label="Copy the command" />
    </div>
  )
}

// Settings: what the service is, and the way to remove it.
function ServiceSettings({ space, entry, onRemoved }: { space: string; entry: SpaceAddonEntry; onRemoved: () => void }) {
  const [removing, setRemoving] = useState(false)
  const parameters = Object.entries(entry.parameters ?? {}).filter(([, v]) => v !== undefined && v !== null && v !== '')
  return (
    <>
      <Section className="space-y-3">
        <SectionHeader title="Service" />
        <dl className="grid grid-cols-[10rem_minmax(0,1fr)] gap-x-4 gap-y-2.5">
          <Fact label="Name">
            <span className="font-mono">{entry.name}</span>
          </Fact>
          {entry.type && (
            <Fact label="Type">
              <span className="font-mono">{entry.type}</span>
            </Fact>
          )}
          {entry.protocol && entry.protocol !== entry.type && (
            <Fact label="Speaks">
              <span className="font-mono">{entry.protocol}</span>
            </Fact>
          )}
          {parameters.map(([key, value]) => (
            <Fact key={key} label={key}>
              <span className="font-mono">{typeof value === 'object' ? JSON.stringify(value) : String(value)}</span>
            </Fact>
          ))}
          {entry.state && entry.state !== 'ready' && (
            <Fact label="State">
              <StateChip state={entry.state} />
              {entry.message && <span className="ml-2 text-xs text-muted">{entry.message}</span>}
            </Fact>
          )}
        </dl>
      </Section>
      <Section tone="danger" className="space-y-3">
        <SectionHeader
          title="Remove"
          actions={<IconButton icon={Trash2} kind="danger" label="Remove service…" onClick={() => setRemoving(true)} />}
          description={
            <>
              Takes <span className="font-mono">{entry.name}</span> out of {space}. Connected apps restart without its
              variables.
            </>
          }
        />
      </Section>
      {removing && (
        <RemoveServiceDialog space={space} service={entry} onClose={() => setRemoving(false)} onRemoved={onRemoved} />
      )}
    </>
  )
}

function Fact({ label, children }: { label: string; children: ReactNode }) {
  return (
    <>
      <dt className="text-xs text-muted">{label}</dt>
      <dd className="min-w-0 text-sm text-ink">{children}</dd>
    </>
  )
}

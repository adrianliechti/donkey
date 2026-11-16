import { useCallback, useState, type ReactNode } from 'react'
import { Tab, TabGroup, TabList, TabPanel, TabPanels } from '@headlessui/react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { useNavigate, useRouter } from '@tanstack/react-router'
import { AppWindow, Check, Folder, FolderPlus, Power, Trash2, TriangleAlert } from 'lucide-react'
import type { Machine, MachineSource, OpenFolder } from './api'
import { AppIcon } from './AppIcon'
import { Modal, ModalBody, ModalFooter, ModalHeader, ModalNotice } from './dialog'
import { folderNameRE, handToMachine, idleText, OpenMachine, type Handed } from './machineOpen'
import { commitsQuery, machineQuery, machineWorkspaceQuery, mutationError, useFleet, useMachineAction } from './queries'
import {
  Button,
  Callout,
  Chip,
  ErrorNote,
  Field,
  IconButton,
  Input,
  List,
  ListRow,
  SearchField,
  Section,
  SectionHeader,
  Select,
  Spinner,
  StatusLine,
} from './ui'
import { ago } from './format'
import { useMediaQuery } from './hooks'
import { healthOf } from './vocabulary'

// The one thing a machine asks of its owner — which folder — and the dialog
// that asks it. A machine is made on the first folder and started whenever
// one is opened, so this dialog is how a person gets to their machine at
// all: from the overview, from the palette, from an app's Source tab, and on
// the Machine page itself.

const tabs = [
  { id: 'folders', label: 'Folders', icon: Folder },
  { id: 'app', label: 'From an app', icon: AppWindow },
  { id: 'machine', label: 'Machine', icon: Power },
] as const
type TabID = (typeof tabs)[number]['id']

const nameRule = 'A name is made of letters, digits, dots, dashes and underscores, and starts with a letter or a digit.'
const lead = 'text-sm leading-relaxed text-ink-soft'

/**
 * MachineDialog asks which folder the agent should work in, the way the New
 * space dialog asks for a space: a folder the machine has or a new, empty
 * one — one list, with the row that makes one at its end — or one made from
 * an app that runs here. The machine itself — stopping it, giving its disk
 * back — is its last section.
 *
 * What happens to the answer is the caller's: on the Machine page the folder
 * opens, from anywhere else the answer is taken there.
 */
export function MachineDialog({
  source,
  busy = false,
  error = '',
  onOpen,
  onLook,
  onClose,
}: {
  /** An app's version to start on, handed over by its Source tab. */
  source?: MachineSource
  /** An open is under way. */
  busy?: boolean
  error?: string
  onOpen: (request: OpenFolder) => void
  /** Starts a stopped machine only to see what is on its disk. */
  onLook: () => void
  onClose: () => void
}) {
  const desktop = useMediaQuery('(min-width: 640px)')
  // Asked afresh, and watched while the dialog is up: what it offers depends
  // on whether the machine runs and, where it does, on what is on its disk.
  const { data: machine } = useQuery({ ...machineQuery, staleTime: 0, refetchInterval: 5_000 })
  const running = machine?.state === 'running'
  const { data: workspace } = useQuery({ ...machineWorkspaceQuery(machine?.startedAt ?? ''), enabled: running })
  // A running machine says what it holds; a stopped one is remembered.
  const folders = (running ? workspace?.folders : machine?.state === 'stopped' ? machine.folders : undefined) ?? []
  const names = folders.map((folder) => folder.name)
  const open = running ? workspace?.open : undefined

  const [tab, setTab] = useState<TabID>(source ? 'app' : 'folders')

  // A new, empty folder: the last row of the list.
  const [name, setName] = useState('')
  const fresh = name.trim()
  const freshTaken = names.includes(fresh)
  const freshOK = folderNameRE.test(fresh) && !freshTaken

  // A folder made from an app.
  const { data: fleet } = useFleet()
  const apps = (fleet?.apps ?? [])
    .map((app) => ({ ...app, space: app.space || 'default', key: `${app.space || 'default'}/${app.name}` }))
    .sort((a, b) => a.key.localeCompare(b.key))
  const [query, setQuery] = useState('')
  const [chosen, setChosen] = useState(source ? `${source.space}/${source.app}` : '')
  const appKey = chosen || apps[0]?.key || ''
  const [space = '', app = ''] = appKey.split('/')
  const shown = apps.filter((a) => a.key.includes(query.trim().toLowerCase()))
  const commits = useQuery({ ...commitsQuery(space, app), enabled: tab === 'app' && app !== '' })
  const versions = commits.data?.commits ?? []
  const [version, setVersion] = useState(source?.commit ?? '')
  // The newest version, unless another was picked or handed over.
  const commit = version || versions[0]?.id || ''
  // A name that is free: the app's own, or the next one after it.
  let suggestion = app
  for (let n = 2; names.includes(suggestion); n++) suggestion = `${app}-${n}`
  const [typed, setTyped] = useState<string | null>(null)
  const target = (typed ?? suggestion).trim()
  const targetTaken = names.includes(target)
  const targetOK = folderNameRE.test(target) && !targetTaken && commit !== ''

  function pickApp(next: string) {
    setChosen(next)
    setVersion('')
    setTyped(null)
  }
  const create = () => !busy && freshOK && onOpen({ folder: fresh, create: true })
  const openApp = () => !busy && targetOK && onOpen({ folder: target, source: { space, app, commit } })

  // What opening a folder does to the machine, said once, where the buttons are.
  const consequence = !machine
    ? undefined
    : machine.state === 'absent'
      ? 'A machine is made for the first folder you open.'
      : machine.state === 'stopped' || machine.state === 'failed'
        ? 'Your machine is started when a folder is opened.'
        : open
          ? `Opening another folder ends what is running in ${open}.`
          : undefined

  return (
    <Modal onClose={() => !busy && onClose()} size="lg" height="medium">
      <ModalHeader
        eyebrow="Machine"
        title={
          source ? (
            <>
              Open <span className="font-mono">{source.app}</span> in your machine
            </>
          ) : (
            'Open a folder'
          )
        }
        onClose={onClose}
        closeDisabled={busy}
      />
      <ModalNotice message={error} />
      <ModalBody padded={false}>
        <TabGroup
          vertical={desktop}
          selectedIndex={tabs.findIndex((t) => t.id === tab)}
          onChange={(index) => setTab(tabs[index].id)}
          className="flex min-h-0 flex-1 flex-col sm:flex-row"
        >
          <TabList
            aria-label="Where to work"
            className="flex shrink-0 gap-0.5 border-b border-line px-3 py-2 sm:w-44 sm:flex-col sm:border-b-0 sm:border-r sm:py-4"
          >
            {tabs.map(({ id, label, icon: Icon }) => (
              <Tab
                key={id}
                className="group flex h-8 flex-1 items-center justify-center gap-2 rounded-md px-2.5 text-sm text-muted transition-colors hover:text-ink focus-visible:outline-2 focus-visible:outline-accent-strong data-selected:bg-raised data-selected:font-medium data-selected:text-ink sm:flex-none sm:justify-start"
              >
                <Icon className="size-4 shrink-0 text-muted group-data-selected:text-ink" aria-hidden />
                <span className="truncate">{label}</span>
                {id === 'folders' && folders.length > 0 && (
                  <span className="ml-auto hidden font-mono text-xs text-muted sm:inline">{folders.length}</span>
                )}
              </Tab>
            ))}
          </TabList>
          <TabPanels className="min-h-0 min-w-0 flex-1 overflow-y-auto px-5 py-5">
            <TabPanel className="space-y-3 focus:outline-none">
              <p className={lead}>
                {folders.length === 0
                  ? machine?.state === 'stopped'
                    ? 'What is on a stopped machine is not known here. Name a folder to make one.'
                    : 'The agent works in a folder. Name one to begin.'
                  : machine?.state === 'stopped'
                    ? 'What your machine had on its disk when it last ran, the folder used last first.'
                    : 'On your machine’s disk, the folder used last first.'}
              </p>
              <List>
                {folders.map((folder) => (
                  <ListRow
                    key={folder.name}
                    actions={
                      folder.name === open ? (
                        <span className="mr-1.5">
                          <Chip>open</Chip>
                        </span>
                      ) : (
                        <Button
                          kind="quiet"
                          size="sm"
                          onClick={() => onOpen({ folder: folder.name })}
                          disabled={busy}
                          className="mr-1.5"
                        >
                          Open
                        </Button>
                      )
                    }
                  >
                    <Folder className="size-4 shrink-0 text-muted" aria-hidden />
                    <span className="min-w-0 flex-1 truncate font-mono text-sm text-ink">{folder.name}</span>
                    <span className="shrink-0 text-xs text-muted" title={new Date(folder.modified).toLocaleString()}>
                      {ago(folder.modified)} ago
                    </span>
                  </ListRow>
                ))}
                {/* A new, empty folder is one more row: named where it will
                    stand, like a domain added to a space. */}
                <ListRow
                  className="bg-surface/40"
                  actions={
                    <Button
                      kind={folders.length === 0 ? 'primary' : 'quiet'}
                      size="sm"
                      onClick={create}
                      disabled={busy || !freshOK}
                      className="mr-1.5"
                    >
                      Create and open
                    </Button>
                  }
                >
                  <FolderPlus className="size-4 shrink-0 text-muted" aria-hidden />
                  <Input
                    bare
                    aria-label="New folder"
                    autoFocus={folders.length === 0}
                    value={name}
                    placeholder="New folder — my-project"
                    autoCapitalize="none"
                    autoComplete="off"
                    spellCheck={false}
                    disabled={busy}
                    onChange={(event) => setName(event.target.value)}
                    onKeyDown={(event) => {
                      if (event.key === 'Enter') {
                        event.preventDefault()
                        create()
                      }
                    }}
                    className="flex-1"
                  />
                </ListRow>
              </List>
              {fresh !== '' && !freshOK && (
                <p className="text-xs leading-relaxed text-muted">
                  {freshTaken
                    ? `There already is a folder named ${fresh} — open it above, or choose another name.`
                    : nameRule}
                </p>
              )}
              {machine?.state === 'stopped' && (
                <p className="text-xs leading-relaxed text-muted">
                  <button
                    type="button"
                    onClick={onLook}
                    disabled={busy}
                    className="rounded text-ink-soft underline decoration-line-strong underline-offset-2 hover:text-ink focus-visible:outline-2 focus-visible:outline-accent-strong disabled:opacity-40"
                  >
                    Start the machine
                  </button>{' '}
                  to see what is on its disk now.
                </p>
              )}
            </TabPanel>

            <TabPanel className="space-y-4 focus:outline-none">
              {apps.length === 0 ? (
                <p className={lead}>No app runs here yet. Once one is deployed, its source can be opened from here.</p>
              ) : (
                <>
                  <p className={lead}>
                    An app’s source, copied into a new folder as the first commit of a repository there — so everything
                    changed from then on reads as a change.
                  </p>
                  {apps.length > 6 && (
                    <SearchField value={query} onChange={setQuery} placeholder="Find an app…" className="w-full" />
                  )}
                  <ul
                    aria-label="App"
                    className="max-h-60 divide-y divide-line/60 overflow-y-auto rounded-lg border border-line"
                  >
                    {shown.map((a) => {
                      const on = a.key === appKey
                      return (
                        <li key={a.key}>
                          <button
                            type="button"
                            aria-pressed={on}
                            onClick={() => pickApp(a.key)}
                            disabled={busy}
                            className={`flex min-h-10 w-full items-center gap-3 px-3 py-1.5 text-left transition-colors focus-visible:outline-2 focus-visible:-outline-offset-2 focus-visible:outline-accent-strong ${
                              on ? 'bg-accent-soft/60' : 'hover:bg-raised'
                            }`}
                          >
                            <AppIcon health={healthOf(a.healthy, a.ready)} stack={a.stack} />
                            <span className="min-w-0 flex-1 truncate font-mono text-sm text-ink">{a.name}</span>
                            <span className="shrink-0 truncate font-mono text-xs text-muted">{a.space}</span>
                            <Check className={`size-4 shrink-0 text-accent ${on ? '' : 'invisible'}`} aria-hidden />
                          </button>
                        </li>
                      )
                    })}
                    {shown.length === 0 && <li className="px-3 py-2.5 text-sm text-muted">No app matches.</li>}
                  </ul>
                  {/* The latest version is what one nearly always wants, so it
                      is what is taken unless another is asked for: a quiet
                      line under the app, not a field of its own. */}
                  <div className="-mt-2 flex min-h-8 flex-wrap items-center gap-x-1 text-xs text-muted">
                    {commits.error ? (
                      'Its versions could not be read.'
                    ) : commits.isPending ? (
                      'Looking up its versions…'
                    ) : versions.length === 0 && commit === '' ? (
                      'No source is recorded for this app yet.'
                    ) : (
                      <>
                        Version
                        <Select
                          variant="quiet"
                          value={commit}
                          onChange={setVersion}
                          options={
                            versions.length > 0
                              ? versions.map((c, index) => ({
                                  value: c.id,
                                  label: `${index === 0 ? 'latest · ' : ''}${c.id.slice(0, 7)} · ${c.message.split('\n')[0].slice(0, 40)}${c.live ? ' · live' : ''}${index === 0 ? '' : ` · ${ago(c.date)} ago`}`,
                                }))
                              : [{ value: commit, label: commit.slice(0, 7) }]
                          }
                          ariaLabel="Version"
                          disabled={busy || versions.length < 2}
                        />
                      </>
                    )}
                  </div>
                  <Field
                    label="Folder"
                    hint={
                      targetTaken
                        ? `There already is a folder named ${target}.`
                        : !folderNameRE.test(target)
                          ? nameRule
                          : `Made at /data/workspace/${target}.`
                    }
                  >
                    <Input
                      value={typed ?? suggestion}
                      onChange={(event) => setTyped(event.target.value)}
                      onKeyDown={(event) => event.key === 'Enter' && openApp()}
                      disabled={busy}
                      autoComplete="off"
                      spellCheck={false}
                    />
                  </Field>
                  {names.includes(app) && (
                    <Callout
                      tone="info"
                      icon={Folder}
                      title={
                        <>
                          Your machine has a folder named <span className="font-mono">{app}</span>
                        </>
                      }
                      action={
                        <Button kind="quiet" size="sm" onClick={() => onOpen({ folder: app })} disabled={busy}>
                          Open it
                        </Button>
                      }
                    >
                      Opening it continues where you left off. A new folder puts this version beside it.
                    </Callout>
                  )}
                </>
              )}
            </TabPanel>

            <TabPanel className="space-y-5 focus:outline-none">
              {machine ? (
                <MachineSection machine={machine} open={open} />
              ) : (
                <Spinner label="Looking for your machine…" />
              )}
            </TabPanel>
          </TabPanels>
        </TabGroup>
      </ModalBody>
      <ModalFooter note={tab === 'machine' ? undefined : consequence}>
        <Button kind="quiet" onClick={onClose} disabled={busy}>
          {tab === 'app' ? 'Cancel' : 'Close'}
        </Button>
        {tab === 'app' && apps.length > 0 && (
          <Button onClick={openApp} disabled={busy || !targetOK}>
            Open
          </Button>
        )}
      </ModalFooter>
    </Modal>
  )
}

// The machine itself: how it is, and the two things there are to do to one
// that looks after itself — stop it now, or give its disk back.
function MachineSection({ machine, open }: { machine: Machine; open?: string }) {
  const stop = useMachineAction('stop')
  const remove = useMachineAction('delete')
  const [confirming, setConfirming] = useState(false)
  const busy = stop.isPending || remove.isPending
  const live = machine.state === 'running' || machine.state === 'starting' || machine.state === 'failed'
  const status = {
    absent: { tone: 'muted', label: 'No machine yet', text: 'One is made when you open a folder.' },
    stopped: { tone: 'muted', label: 'Stopped', text: 'Its disk is kept, and opening a folder starts it.' },
    starting: { tone: 'accent', label: 'Starting', text: '' },
    running: {
      tone: 'success',
      label: 'Running',
      text: open ? `The agent has ${open} open.` : 'No folder is open.',
    },
    failed: { tone: 'danger', label: 'Not coming up', text: machine.message ?? '' },
  }[machine.state] as { tone: 'muted' | 'accent' | 'success' | 'danger'; label: string; text: string }

  return (
    <>
      <div className="space-y-1.5">
        <StatusLine tone={status.tone} label={status.label} busy={machine.state === 'starting'} size="md" />
        {status.text && <p className="text-sm leading-relaxed break-words text-ink-soft">{status.text}</p>}
        <p className="text-xs leading-relaxed text-muted">
          {machine.idleMinutes
            ? `A machine stops by itself when it goes unused for ${idleText(machine.idleMinutes)}, and keeps its disk`
            : 'A machine keeps its disk when it stops'}
          {machine.storage ? ` (${machine.storage}).` : '.'}
          {machine.egress === 'restricted' &&
            ' It has no way out to the internet: packages install from the installation’s mirror.'}
        </p>
      </div>
      <ErrorNote
        message={mutationError(stop, 'Stopping did not work.') || mutationError(remove, 'Deleting did not work.')}
      />
      {live && (
        <Section>
          <SectionHeader
            title="Stop"
            actions={
              <Button kind="quiet" size="sm" onClick={() => stop.mutate()} disabled={busy}>
                {stop.isPending ? 'Stopping…' : 'Stop machine'}
              </Button>
            }
            description="Ends what is running in it — agent turns, terminals, servers. The disk is kept, and opening a folder starts the machine again."
          />
        </Section>
      )}
      {machine.state !== 'absent' && (
        <Section tone="danger" className="space-y-3">
          <SectionHeader
            title="Delete"
            actions={
              !confirming && (
                <IconButton
                  icon={Trash2}
                  kind="danger"
                  label="Delete machine…"
                  onClick={() => setConfirming(true)}
                  disabled={busy}
                />
              )
            }
            description="Gives the machine and its disk back. A new one is made when you open a folder again."
          />
          {confirming && (
            <>
              <Callout tone="danger" icon={TriangleAlert} title="Delete your machine and its disk?">
                Your home, your folders and everything you installed are removed. This cannot be undone.
              </Callout>
              <div className="flex justify-end gap-2">
                <Button kind="quiet" onClick={() => setConfirming(false)} disabled={busy}>
                  Cancel
                </Button>
                <Button
                  kind="danger"
                  onClick={() => remove.mutate(undefined, { onSuccess: () => setConfirming(false) })}
                  disabled={busy}
                >
                  {remove.isPending ? 'Deleting…' : 'Delete machine and its disk'}
                </Button>
              </div>
            </>
          )}
        </Section>
      )}
    </>
  )
}

/**
 * OpenMachineProvider is the way to one's machine from anywhere in the
 * console (useOpenMachine). Where the agent already has a folder open, that
 * is where it goes. Otherwise — and always for an app's version handed over
 * by its Source tab — it asks which folder first, in a dialog over the page
 * one is on, so changing one's mind costs nothing: no machine is made or
 * started until a folder is named. The answer is taken to the Machine page,
 * which does the rest and shows it happening.
 */
export function OpenMachineProvider({ children }: { children: ReactNode }) {
  const client = useQueryClient()
  const navigate = useNavigate()
  const router = useRouter()
  const [asking, setAsking] = useState<{ source?: MachineSource } | null>(null)

  const open = useCallback(
    async (source?: MachineSource) => {
      if (source) {
        setAsking({ source })
        return
      }
      // On the Machine page itself there is nowhere to go, and the page asks.
      if (router.state.location.pathname !== '/machine') {
        try {
          const machine = await client.fetchQuery({ ...machineQuery, staleTime: 0 })
          if (machine.enabled && machine.allowed) {
            if (machine.state === 'absent' || machine.state === 'stopped') {
              setAsking({})
              return
            }
            if (machine.state === 'running') {
              const workspace = await client.fetchQuery(machineWorkspaceQuery(machine.startedAt ?? ''))
              if (!workspace.open) {
                setAsking({})
                return
              }
            }
          }
        } catch {
          // The Machine page says what is wrong, and what to do about it.
        }
      }
      void navigate({ to: '/machine' })
    },
    [client, navigate, router],
  )
  // Only an app's version counts as one: a click's event is not.
  const launch = useCallback(
    (source?: MachineSource) => void open(source && typeof source.commit === 'string' ? source : undefined),
    [open],
  )

  function take(what: Handed) {
    handToMachine(what)
    setAsking(null)
    void navigate({ to: '/machine' })
  }

  return (
    <OpenMachine.Provider value={launch}>
      {children}
      {asking && (
        <MachineDialog
          source={asking.source}
          onOpen={take}
          onLook={() => take('look')}
          onClose={() => setAsking(null)}
        />
      )}
    </OpenMachine.Provider>
  )
}

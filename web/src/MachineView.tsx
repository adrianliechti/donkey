import { useEffect, useRef, useState, type ReactNode } from 'react'
import { useQuery } from '@tanstack/react-query'
import { Navigate } from '@tanstack/react-router'
import { Menu, MenuButton, MenuItem, MenuItems } from '@headlessui/react'
import {
  Clock,
  EllipsisVertical,
  ExternalLink,
  FolderCode,
  FolderOpen,
  Info,
  RotateCw,
  Square,
  Trash2,
  TriangleAlert,
} from 'lucide-react'
import type { MachineState, OpenFolder } from './api'
import { Modal, ModalBody, ModalFooter, ModalHeader } from './dialog'
import { machineQuery, machineWorkspaceQuery, mutationError, useMachineAction, useOpenMachineFolder } from './queries'
import { HeaderActions } from './HeaderActions'
import { Button, Callout, EmptyState, ErrorNote, IconLink, Page, panelClass, Spinner, wellClass } from './ui'
import { errText } from './format'
import { useNow } from './hooks'
import { MachineDialog } from './MachineFolders'
import { clearHandedToMachine, handedToMachine, idleText } from './machineOpen'

type Confirm = 'restart' | 'stop' | 'delete'

// The sign-in inside a machine is worth a word once it has days left, not
// weeks: a restart renews it, and a restart ends what is running.
const SIGN_IN_WARNING_MS = 3 * 24 * 60 * 60 * 1000

// A folder to open as soon as there is a machine to open it in: the one that
// was open before a restart, one picked while the machine was stopped, or
// what another page handed over (an app's version, from its Source tab).
type Pending = {
  open: OpenFolder
  // When the container that must not get this request came up. A restart is
  // asked of a machine that still runs, and for a moment it still answers.
  after?: string
}

/**
 * The signed-in person's machine: a development container of their own, with
 * the coding agent's interface shown right here once it has a folder open.
 *
 * One thing is asked of its owner — which folder — in a popup, here and
 * wherever the console offers the way to a machine (MachineDialog).
 * The machine follows from the answer: it is made on the first folder and
 * started whenever one is opened and it is not running. A machine stops by
 * itself when nobody uses it, so stopped is how its owner usually finds it;
 * what its disk held is remembered for that, and offered before anything is
 * started. Without a machine there is nothing for this page to show, and it
 * gives way to the overview.
 *
 * The agent is embedded from the machine's own address. That is a different
 * origin from the console's, so nothing running in a machine can reach this
 * page or the session behind it; the gateway lets only the machine's owner
 * through, and only the console frame it.
 */
export function MachineView() {
  const {
    data: machine,
    error,
    isPending,
  } = useQuery({
    ...machineQuery,
    staleTime: 0,
    // Watched closely while it comes up, lazily once it runs or rests.
    refetchInterval: (query) => (query.state.data?.state === 'starting' ? 2_000 : 10_000),
  })
  const start = useMachineAction('start')
  const stop = useMachineAction('stop')
  const remove = useMachineAction('delete')
  const [confirm, setConfirm] = useState<Confirm | null>(null)
  const [choosing, setChoosing] = useState(false)
  const [menuOpen, setMenuOpen] = useState(false)
  // What another page handed over — a folder, or only the wish to look — is
  // this page's first thing to do. It is taken once: coming back here later
  // opens and starts nothing by itself.
  const [pending, setPending] = useState<Pending | null>(() => {
    const handed = handedToMachine()
    return handed && handed !== 'look' ? { open: handed } : null
  })
  const arrived = useRef(handedToMachine() !== null)
  useEffect(() => clearHandedToMachine(), [])
  const now = useNow(60_000)

  // A start answers only once the runtime has taken the machine — which on a
  // first start includes fetching its image — so the page says "starting"
  // from the click on, not from the answer.
  const state: MachineState = start.isPending ? 'starting' : (machine?.state ?? 'absent')
  const running = state === 'running' && !!machine?.url
  const started = machine?.startedAt ?? ''

  // The folders are the machine's to answer for, so they are asked for only
  // while it runs: closely while the agent comes up in one, lazily after —
  // which is how an agent that stopped on its own is noticed.
  const workspaceQuery = useQuery({
    ...machineWorkspaceQuery(started),
    enabled: running,
    refetchInterval: (query) => (query.state.data?.open && !query.state.data.ready ? 1_000 : 10_000),
  })
  const openFolder = useOpenMachineFolder(started)
  const workspace = running ? workspaceQuery.data : undefined

  // The pending folder is sent once, to a machine that came up after it was
  // asked for.
  const sent = useRef<Pending | null>(null)
  const wentDown = useRef(false)
  const mutateOpen = openFolder.mutate
  const machineState = machine?.state
  useEffect(() => {
    if (!pending || sent.current === pending) return
    if (machineState !== 'running') {
      wentDown.current = true
      return
    }
    if (start.isPending) return
    if (pending.after !== undefined && started === pending.after && !wentDown.current) return
    sent.current = pending
    mutateOpen(pending.open, { onSettled: () => setPending(null) })
  }, [pending, machineState, started, start.isPending, mutateOpen])

  // What was handed over needs a machine: one that does not run is started
  // for it, as soon as this page knows which it is.
  const startMachine = start.mutate
  useEffect(() => {
    if (!arrived.current || !machineState) return
    arrived.current = false
    if (machineState === 'absent' || machineState === 'stopped' || machineState === 'failed') {
      startMachine(undefined, { onError: () => setPending(null) })
    }
  }, [machineState, startMachine])

  const busy = start.isPending || stop.isPending || remove.isPending
  const failure =
    mutationError(start, 'Starting did not work.') ||
    mutationError(stop, 'Stopping did not work.') ||
    mutationError(remove, 'Deleting did not work.')
  const openFailure = mutationError(openFolder, 'The folder could not be opened.')

  function later(next: Pending | null) {
    wentDown.current = false
    sent.current = null
    setPending(next)
  }

  function reset() {
    start.reset()
    stop.reset()
    remove.reset()
    openFolder.reset()
  }

  // Starting is never asked for by itself: it is what opening a folder does
  // to a machine that is not running. `request` is what to open once it is
  // up — nothing, for a machine started only to look at its disk.
  function startFor(request: OpenFolder | null) {
    later(request ? { open: request, after: started } : null)
    start.mutate(undefined, { onError: () => later(null) })
  }

  function run(action: Confirm) {
    setConfirm(null)
    reset()
    if (action === 'restart') {
      // A restart comes back to the folder it left.
      startFor(workspace?.open ? { folder: workspace.open } : null)
    } else {
      later(null)
      if (action === 'stop') stop.mutate()
      else remove.mutate()
    }
  }

  // The one thing the page asks. In a machine that runs, the folder opens;
  // in one that does not, the machine is made or started for it first.
  function open(request: OpenFolder) {
    reset()
    if (running) openFolder.mutate(request)
    else startFor(request)
  }

  if (isPending) {
    return (
      <Page>
        <Spinner label="Looking for your machine…" />
      </Page>
    )
  }
  if (!machine) {
    return (
      <Page>
        <ErrorNote message={errText(error, 'The machine could not be read.')} />
      </Page>
    )
  }
  if (!machine.enabled || !machine.allowed) {
    return (
      <Page>
        <div className={panelClass}>
          <EmptyState icon={FolderCode} title="No machine for you here">
            {machine.enabled
              ? 'Machines are for people who build here. Ask a space admin to make you a developer of a space.'
              : 'This installation has no machines. An administrator turns them on in the install settings.'}
          </EmptyState>
        </div>
      </Page>
    )
  }

  // No machine, and none on its way: this page has nothing to show. The way
  // to a machine is the popup that asks for a folder, on the overview — which
  // is also where deleting one's machine leads back to. A start that failed
  // stays, to say why.
  if (state === 'absent' && !pending && !failure) return <Navigate to="/" replace />

  const agent = running && workspace?.open && workspace.ready ? workspace.open : null
  // A machine that is not coming up has nothing to open a folder in; the page
  // says so, and starting it again asks anew.
  const waiting = state === 'failed' ? null : pending
  // The folder on its way: asked for and not yet sent, being made, or made
  // and waiting for the agent to answer in it.
  const opening =
    waiting?.open.folder ??
    (openFolder.isPending ? openFolder.variables.folder : undefined) ??
    (workspace?.open && !workspace.ready ? workspace.open : undefined)
  const signInEnds = machine.signInExpires ? new Date(machine.signInExpires) : null
  const signInSoon = running && signInEnds !== null && signInEnds.getTime() - now < SIGN_IN_WARNING_MS

  // The popup that asks for a folder: for the first one, for another one,
  // and for the one to start a stopped machine with.
  const ask = () => {
    openFolder.reset()
    setChoosing(true)
  }

  // The page has no title row. What it is, the frame or the panel says
  // itself; what belongs to the open folder sits in the console's top bar,
  // so the agent gets every line of height under it. What can be done to
  // the machine itself is a menu behind them: it looks after itself, and
  // those are for the rare day it should not.
  const actions = state !== 'absent' && (
    <HeaderActions>
      {agent && (
        <Button kind="quiet" size="sm" icon={FolderOpen} title="Open another folder…" onClick={ask} disabled={busy}>
          <span className="max-w-40 truncate font-mono">{agent}</span>
        </Button>
      )}
      {agent && machine.url && (
        <IconLink icon={ExternalLink} label="Open the machine in a tab of its own" href={machine.url} />
      )}
      <MachineMenu state={state} disabled={busy} onPick={setConfirm} onOpenChange={setMenuOpen} />
    </HeaderActions>
  )
  const dialogs = (
    <>
      {confirm && (
        <ConfirmDialog
          action={confirm}
          folder={agent}
          onCancel={() => setConfirm(null)}
          onConfirm={() => run(confirm)}
        />
      )}
      {choosing && (
        <MachineDialog
          busy={openFolder.isPending}
          error={openFailure}
          onClose={() => setChoosing(false)}
          onOpen={(request) => {
            // In a machine that runs, the popup stays until the folder is
            // open, so a refusal is read where the name was typed. One that
            // has to start first shows that on the page instead.
            if (running) openFolder.mutate(request, { onSuccess: () => setChoosing(false) })
            else {
              setChoosing(false)
              open(request)
            }
          }}
          onLook={() => {
            setChoosing(false)
            reset()
            startFor(null)
          }}
        />
      )}
    </>
  )

  if (agent && machine.url) {
    return (
      <Page bleed>
        {actions}
        {/* Only when there is something to say; otherwise the agent starts
            right under the bar. */}
        {(failure || machine.outdated || signInSoon) && (
          <div className="shrink-0 space-y-2 border-b border-line px-6 py-3">
            <ErrorNote message={failure} />
            {machine.outdated && (
              <Callout
                tone="info"
                icon={Info}
                title="A newer machine image is available"
                action={
                  <Button kind="quiet" size="sm" onClick={() => setConfirm('restart')} disabled={busy}>
                    Restart…
                  </Button>
                }
              >
                Restarting picks it up. Your disk stays as it is.
              </Callout>
            )}
            {signInSoon && signInEnds && (
              <Callout
                tone="warning"
                icon={Clock}
                title={`The donkey sign-in inside this machine ends ${signInEnds.toLocaleString()}`}
                action={
                  <Button kind="quiet" size="sm" onClick={() => setConfirm('restart')} disabled={busy}>
                    Restart…
                  </Button>
                }
              >
                After that, <Mono>donkey</Mono> commands in the machine stop working until it is restarted.
              </Callout>
            )}
          </div>
        )}
        {/* Keyed by the folder and the container: another folder is another
            agent, and its interface starts over. While something of the
            console's is open over it — the menu, a dialog — the frame is
            inert: the agent puts the cursor in its own prompt when it likes
            (as it loads, for one), and a frame taking the focus reads to the
            console as a click outside, which would close what was just
            opened. */}
        <iframe
          key={`${started}/${agent}`}
          src={machine.url}
          title="Wingman, in your machine"
          inert={menuOpen || choosing || confirm !== null}
          className="min-h-0 w-full flex-1 border-0"
          allow="clipboard-read; clipboard-write; fullscreen"
          referrerPolicy="no-referrer"
        />
        {dialogs}
      </Page>
    )
  }

  // Everything else is one panel: what is under way before the agent can be
  // shown, or — with nothing open — the way to the popup that asks.
  const fetching = waiting?.open.source || openFolder.variables?.source
  const unread = running && !workspace && workspaceQuery.error
  let body: ReactNode
  if (state === 'failed') {
    body = (
      <div className="space-y-3 p-4">
        <Callout
          tone="danger"
          icon={TriangleAlert}
          title="The machine isn't coming up"
          action={
            <Button kind="quiet" size="sm" icon={RotateCw} onClick={() => run('restart')} disabled={busy}>
              Start again
            </Button>
          }
        >
          Your disk is untouched. What the machine said last:
        </Callout>
        {machine.message && <pre className={`${wellClass} max-h-80 whitespace-pre-wrap`}>{machine.message}</pre>}
      </div>
    )
  } else if (state === 'starting') {
    body = (
      <Waiting label="Starting your machine…">
        {opening && (
          <span className="mb-2 block">
            <Mono>{opening}</Mono> opens as soon as it is up.
          </span>
        )}
        The first start fetches the machine image, which can take a few minutes.
        {machine.message && <span className="mt-2 block font-mono text-xs break-words">{machine.message}</span>}
      </Waiting>
    )
  } else if (opening) {
    body = (
      <Waiting label={`Opening ${opening}…`}>
        {fetching ? 'Its source is fetched from donkey first.' : 'The agent is starting in it.'}
      </Waiting>
    )
  } else if (unread) {
    body = (
      <div className="space-y-3 p-4">
        <Callout
          tone="warning"
          icon={TriangleAlert}
          title="The machine's folders could not be read"
          action={
            <Button kind="quiet" size="sm" icon={RotateCw} onClick={() => setConfirm('restart')} disabled={busy}>
              Restart…
            </Button>
          }
        >
          {errText(workspaceQuery.error, 'The machine did not answer.')}
        </Callout>
      </div>
    )
  } else if (running && !workspace) {
    body = <Spinner label="Looking at your machine's folders…" />
  } else {
    body = (
      <EmptyState
        icon={FolderCode}
        title={
          state === 'absent' ? 'No machine yet' : state === 'stopped' ? 'Your machine is stopped' : 'No folder is open'
        }
        action={
          <Button icon={FolderOpen} onClick={ask} disabled={busy}>
            Open a folder…
          </Button>
        }
      >
        {state === 'absent' && 'It could not be made. Opening a folder tries again.'}
        {state === 'stopped' && (
          <>
            Its disk is kept, and opening a folder starts it.
            {machine.idleMinutes
              ? ` A machine stops by itself when it goes unused for ${idleText(machine.idleMinutes)}.`
              : ''}
          </>
        )}
        {state === 'running' && (workspace?.message || 'The agent works in one folder at a time.')}
      </EmptyState>
    )
  }

  return (
    <Page>
      {actions}
      <ErrorNote message={failure || (choosing ? '' : openFailure)} />
      <div className={panelClass}>{body}</div>
      {dialogs}
    </Page>
  )
}

// What can be done to the machine itself, behind one button: it starts when a
// folder is opened and stops when nobody uses it, so these are for the day
// it should not — a restart for a new image or sign-in, a stop right now,
// and giving the disk back.
function MachineMenu({
  state,
  disabled,
  onPick,
  onOpenChange,
}: {
  state: MachineState
  disabled: boolean
  onPick: (action: Confirm) => void
  /** Told when the menu opens and closes. */
  onOpenChange: (open: boolean) => void
}) {
  const item =
    'flex min-h-9 w-full items-center gap-2.5 px-3 text-left text-sm text-ink-soft data-focus:bg-accent-soft data-focus:text-ink'
  const live = state === 'running' || state === 'starting' || state === 'failed'
  return (
    <Menu>
      {({ open }) => (
        <>
          <Reports value={open} to={onOpenChange} />
          <MachineMenuParts state={state} disabled={disabled} onPick={onPick} item={item} live={live} />
        </>
      )}
    </Menu>
  )
}

// Reports tells a parent what a render prop knows — here, that a menu is open.
function Reports({ value, to }: { value: boolean; to: (value: boolean) => void }) {
  useEffect(() => {
    to(value)
    return () => to(false)
  }, [value, to])
  return null
}

function MachineMenuParts({
  state,
  disabled,
  onPick,
  item,
  live,
}: {
  state: MachineState
  disabled: boolean
  onPick: (action: Confirm) => void
  item: string
  live: boolean
}) {
  return (
    <>
      <MenuButton
        title="Machine actions"
        aria-label="Machine actions"
        disabled={disabled}
        className="flex size-8 shrink-0 items-center justify-center rounded-md text-muted hover:bg-line/60 hover:text-ink focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-accent-strong disabled:pointer-events-none disabled:opacity-40 data-open:bg-line/60 data-open:text-ink"
      >
        <EllipsisVertical className="size-4" aria-hidden />
      </MenuButton>
      <MenuItems
        anchor="bottom end"
        className="z-50 mt-1 w-64 overflow-hidden rounded-lg border border-line bg-card py-1 shadow-xl focus:outline-none"
      >
        {(state === 'running' || state === 'failed') && (
          <MenuItem>
            <button type="button" onClick={() => onPick('restart')} className={item}>
              <RotateCw className="size-4" aria-hidden /> Restart machine…
            </button>
          </MenuItem>
        )}
        {live && (
          <>
            <MenuItem>
              <button type="button" onClick={() => onPick('stop')} className={item}>
                <Square className="size-4" aria-hidden /> Stop machine…
              </button>
            </MenuItem>
            <div className="my-1 h-px bg-line" aria-hidden />
          </>
        )}
        <MenuItem>
          <button
            type="button"
            onClick={() => onPick('delete')}
            className={`${item} text-danger data-focus:bg-danger/10 data-focus:text-danger`}
          >
            <Trash2 className="size-4" aria-hidden /> Delete machine and its disk…
          </button>
        </MenuItem>
      </MenuItems>
    </>
  )
}

// A centred "something is under way" for the panel that otherwise holds the
// agent or a choice.
function Waiting({ label, children }: { label: string; children?: ReactNode }) {
  return (
    <div className="px-6 py-12 text-center">
      <div className="flex justify-center">
        <Spinner inline label={label} />
      </div>
      {children && <p className="mx-auto mt-2 max-w-md text-sm text-muted">{children}</p>}
    </div>
  )
}

function Mono({ children }: { children: ReactNode }) {
  return <span className="font-mono text-ink-soft">{children}</span>
}

const confirmText: Record<Confirm, { title: string; body: string; action: string }> = {
  restart: {
    title: 'Restart machine',
    body: 'Ends what is running in it — agent turns, terminals, servers. Your disk stays, and the machine comes back with a fresh donkey sign-in and the current image',
    action: 'Restart machine',
  },
  stop: {
    title: 'Stop machine',
    body: 'Ends what is running in it — agent turns, terminals, servers. Your disk is kept, and opening a folder starts the machine again.',
    action: 'Stop machine',
  },
  delete: {
    title: 'Delete machine',
    body: 'Removes the machine and its disk: your home, your folders and everything you installed. This cannot be undone — a machine that is only stopped keeps its disk, and stops by itself when it is not used.',
    action: 'Delete machine and its disk',
  },
}

function ConfirmDialog({
  action,
  folder,
  onCancel,
  onConfirm,
}: {
  action: Confirm
  /** The folder that is open, which a restart comes back to. */
  folder: string | null
  onCancel: () => void
  onConfirm: () => void
}) {
  const text = confirmText[action]
  return (
    <Modal onClose={onCancel} size="sm">
      <ModalHeader title={text.title} onClose={onCancel} />
      <ModalBody>
        {action === 'delete' ? (
          <Callout tone="danger" icon={TriangleAlert}>
            {text.body}
          </Callout>
        ) : (
          <p className="text-sm leading-relaxed text-ink-soft">
            {text.body}
            {action === 'restart' &&
              (folder ? (
                <>
                  , with <Mono>{folder}</Mono> open again.
                </>
              ) : (
                '.'
              ))}
          </p>
        )}
      </ModalBody>
      <ModalFooter>
        <Button kind="quiet" onClick={onCancel}>
          Cancel
        </Button>
        <Button kind={action === 'delete' ? 'danger' : 'primary'} onClick={onConfirm}>
          {text.action}
        </Button>
      </ModalFooter>
    </Modal>
  )
}

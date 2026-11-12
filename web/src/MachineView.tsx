import { useState, type ReactNode } from 'react'
import { useQuery } from '@tanstack/react-query'
import { Clock, ExternalLink, Info, Play, RotateCw, Square, SquareTerminal, Trash2, TriangleAlert } from 'lucide-react'
import type { Machine, MachineState } from './api'
import { Modal, ModalBody, ModalFooter, ModalHeader } from './dialog'
import { machineQuery, mutationError, useMachineAction } from './queries'
import {
  Button,
  Callout,
  EmptyState,
  ErrorNote,
  IconButton,
  LinkButton,
  Page,
  PageBar,
  panelClass,
  Spinner,
  StatusLine,
  wellClass,
} from './ui'
import { errText } from './format'
import { useNow } from './hooks'

type Confirm = 'restart' | 'stop' | 'delete'

// How a state reads in the page bar.
const stateLine: Record<MachineState, { tone: 'success' | 'accent' | 'danger' | 'muted'; label: string }> = {
  running: { tone: 'success', label: 'Running' },
  starting: { tone: 'accent', label: 'Starting' },
  failed: { tone: 'danger', label: 'Not coming up' },
  stopped: { tone: 'muted', label: 'Stopped' },
  absent: { tone: 'muted', label: 'Not created' },
}

// The sign-in inside a machine is worth a word once it has days left, not
// weeks: a restart renews it, and a restart ends what is running.
const SIGN_IN_WARNING_MS = 3 * 24 * 60 * 60 * 1000

/**
 * The signed-in person's machine: a development container of their own, with
 * the coding agent's interface shown right here once it runs.
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
  const now = useNow(60_000)

  const busy = start.isPending || stop.isPending || remove.isPending
  // A start answers only once the runtime has taken the machine — which on a
  // first start includes fetching its image — so the page says "starting"
  // from the click on, not from the answer.
  const state: MachineState = start.isPending ? 'starting' : (machine?.state ?? 'absent')
  const failure =
    mutationError(start, 'Starting did not work.') ||
    mutationError(stop, 'Stopping did not work.') ||
    mutationError(remove, 'Deleting did not work.')

  function run(action: Confirm) {
    setConfirm(null)
    start.reset()
    stop.reset()
    remove.reset()
    if (action === 'restart') start.mutate()
    else if (action === 'stop') stop.mutate()
    else remove.mutate()
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
          <EmptyState icon={SquareTerminal} title="No machine for you here">
            {machine.enabled
              ? 'Machines are for people who build here. Ask a space admin to make you a developer of a space.'
              : 'This installation has no machines. An administrator turns them on in the install settings.'}
          </EmptyState>
        </div>
      </Page>
    )
  }

  const running = state === 'running' && !!machine.url
  const line = stateLine[state]
  const signInEnds = machine.signInExpires ? new Date(machine.signInExpires) : null
  const signInSoon = running && signInEnds !== null && signInEnds.getTime() - now < SIGN_IN_WARNING_MS

  return (
    <Page fill={running}>
      <PageBar
        actions={
          state !== 'absent' && (
            <>
              {running && machine.url && (
                <LinkButton kind="quiet" size="sm" href={machine.url} icon={ExternalLink}>
                  Open in a tab
                </LinkButton>
              )}
              {(state === 'running' || state === 'failed') && (
                <IconButton
                  icon={RotateCw}
                  label="Restart machine…"
                  onClick={() => setConfirm('restart')}
                  disabled={busy}
                />
              )}
              {state !== 'stopped' && (
                <IconButton icon={Square} label="Stop machine…" onClick={() => setConfirm('stop')} disabled={busy} />
              )}
              <IconButton
                icon={Trash2}
                kind="danger"
                label="Delete machine…"
                onClick={() => setConfirm('delete')}
                disabled={busy}
              />
            </>
          )
        }
      >
        <h1 className="font-mono text-base font-bold tracking-tight">Machine</h1>
        <StatusLine tone={line.tone} label={line.label} busy={state === 'starting' || stop.isPending} size="md" />
      </PageBar>

      <ErrorNote message={failure} />
      {running && machine.outdated && (
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

      {running && machine.url ? (
        // The frame fills what the window leaves under the bar; on a narrow
        // window, where the page scrolls as a document, it keeps a height of
        // its own.
        <div className={`${panelClass} min-h-[70dvh] flex-1 overflow-hidden xl:min-h-0`}>
          <iframe
            src={machine.url}
            title="Wingman, in your machine"
            className="size-full border-0"
            allow="clipboard-read; clipboard-write; fullscreen"
            referrerPolicy="no-referrer"
          />
        </div>
      ) : (
        <div className={panelClass}>
          <Resting machine={machine} state={state} busy={busy} onStart={() => run('restart')} />
        </div>
      )}

      {confirm && <ConfirmDialog action={confirm} onCancel={() => setConfirm(null)} onConfirm={() => run(confirm)} />}
    </Page>
  )
}

// What the page shows in place of the agent while the machine is not serving.
function Resting({
  machine,
  state,
  busy,
  onStart,
}: {
  machine: Machine
  state: MachineState
  busy: boolean
  onStart: () => void
}) {
  const size = [machine.cpu && `${machine.cpu} CPU`, machine.memory && `${machine.memory} of memory`]
    .filter(Boolean)
    .join(' and ')

  if (state === 'starting') {
    return (
      <div className="px-6 py-12 text-center">
        <div className="flex justify-center">
          <Spinner inline label="Starting your machine…" />
        </div>
        <p className="mx-auto mt-2 max-w-md text-sm text-muted">
          The first start fetches the machine image, which can take a few minutes.
        </p>
        {machine.message && (
          <p className="mx-auto mt-2 max-w-md font-mono text-xs break-words text-muted">{machine.message}</p>
        )}
      </div>
    )
  }
  if (state === 'failed') {
    return (
      <div className="space-y-3 p-4">
        <Callout
          tone="danger"
          icon={TriangleAlert}
          title="The machine isn't coming up"
          action={
            <Button kind="quiet" size="sm" icon={RotateCw} onClick={onStart} disabled={busy}>
              Start again
            </Button>
          }
        >
          Your disk is untouched. What the machine said last:
        </Callout>
        {machine.message && <pre className={`${wellClass} max-h-80 whitespace-pre-wrap`}>{machine.message}</pre>}
      </div>
    )
  }
  if (state === 'stopped') {
    return (
      <EmptyState
        icon={SquareTerminal}
        title="Your machine is stopped"
        action={
          <Button icon={Play} onClick={onStart} disabled={busy}>
            Start machine
          </Button>
        }
      >
        Its disk is kept: your home and workspace are there when it starts again.
      </EmptyState>
    )
  }
  return (
    <EmptyState
      icon={SquareTerminal}
      title="A machine of your own"
      action={
        <Button icon={Play} onClick={onStart} disabled={busy}>
          Create machine
        </Button>
      }
    >
      <p>
        A development container that is yours alone: Python, Node and Go, the <Mono>donkey</Mono> CLI signed in as you,
        and the Wingman coding agent, opened right here.
      </p>
      <p className="mt-2">
        Your home and workspace live on a {machine.storage ?? ''} disk at <Mono>/data</Mono> and survive restarts.
        {size && <> It may use up to {size}.</>}
      </p>
    </EmptyState>
  )
}

function Mono({ children }: { children: ReactNode }) {
  return <span className="font-mono text-ink-soft">{children}</span>
}

const confirmText: Record<Confirm, { title: string; body: ReactNode; action: string }> = {
  restart: {
    title: 'Restart machine',
    body: 'Ends what is running in it — agent turns, terminals, servers. Your disk stays, and the machine comes back with a fresh donkey sign-in and the current image.',
    action: 'Restart machine',
  },
  stop: {
    title: 'Stop machine',
    body: 'Ends what is running in it — agent turns, terminals, servers. Your disk is kept, so your home and workspace are there when you start it again.',
    action: 'Stop machine',
  },
  delete: {
    title: 'Delete machine',
    body: 'Removes the machine and its disk: your home, your workspace and everything you installed. This cannot be undone — stopping the machine instead keeps the disk.',
    action: 'Delete machine and its disk',
  },
}

function ConfirmDialog({
  action,
  onCancel,
  onConfirm,
}: {
  action: Confirm
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
          <p className="text-sm leading-relaxed text-ink-soft">{text.body}</p>
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

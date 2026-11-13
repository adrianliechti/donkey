import { useMemo, useState } from 'react'
import {
  Dialog,
  DialogBackdrop,
  DialogPanel,
  Combobox,
  ComboboxInput,
  ComboboxOption,
  ComboboxOptions,
} from '@headlessui/react'
import { useQuery } from '@tanstack/react-query'
import { useNavigate } from '@tanstack/react-router'
import { Activity, Boxes, FolderCode, Search, UploadCloud } from 'lucide-react'
import { useDeploy } from './deploy'
import { useOpenMachine } from './machineOpen'
import { machineQuery, useFleet } from './queries'
import { backdropClass } from './dialog'
import { AppIcon } from './AppIcon'
import { healthOf } from './vocabulary'

// One search box for the whole console: ⌘K anywhere, type a few letters, hit
// Enter. It jumps to an app, a space, or one of the standing actions (deploy,
// your machine, system) — the operator's shortcut past the click-through
// when the fleet is more than a screen, and the way to one's machine from
// any page.
type Item = {
  key: string
  label: string
  hint?: string
  icon: React.ReactNode
  // terms are what a query matches against — lowercased, joined.
  terms: string
  run: () => void
}

export function CommandPalette({ open, onClose }: { open: boolean; onClose: () => void }) {
  const navigate = useNavigate()
  const openDeploy = useDeploy()
  const openMachine = useOpenMachine()
  const { data } = useFleet()
  const { data: machine } = useQuery(machineQuery)
  const hasMachine = !!machine?.enabled && machine.allowed
  const [query, setQuery] = useState('')

  // Selecting an item closes the palette through its own onClose, which does
  // not clear the query — so start every open from a clean, unfiltered list.
  const [wasOpen, setWasOpen] = useState(open)
  if (open !== wasOpen) {
    setWasOpen(open)
    if (open) setQuery('')
  }

  const items = useMemo<Item[]>(() => {
    const go = (fn: () => void) => () => {
      onClose()
      fn()
    }
    const apps: Item[] = (data?.apps ?? []).map((app) => {
      const space = app.space || 'default'
      const health = healthOf(app.healthy, app.ready)
      return {
        key: `app:${space}/${app.name}`,
        label: app.name,
        hint: space,
        icon: <AppIcon health={health} stack={app.stack} />,
        terms: `${app.name} ${space}`.toLowerCase(),
        run: go(() =>
          navigate({
            to: '/app/$space/$name/{-$tab}',
            params: { space, name: app.name, tab: undefined },
          }),
        ),
      }
    })
    const spaces: Item[] = (data?.spaces ?? []).map((space) => ({
      key: `space:${space}`,
      label: space,
      hint: 'space',
      icon: <Boxes className="size-4 text-muted" aria-hidden />,
      terms: `${space} space`.toLowerCase(),
      run: go(() => navigate({ to: '/space/$space', params: { space } })),
    }))
    const actions: Item[] = [
      {
        key: 'action:deploy',
        label: 'Deploy an app',
        hint: 'action',
        icon: <UploadCloud className="size-4 text-muted" aria-hidden />,
        terms: 'deploy new app upload',
        run: go(() => openDeploy()),
      },
      {
        key: 'action:machine',
        label: 'Your machine',
        hint: 'action',
        icon: <FolderCode className="size-4 text-muted" aria-hidden />,
        terms: 'your machine agent wingman code folder ide',
        run: go(() => openMachine()),
      },
      {
        key: 'action:system',
        label: 'System health',
        hint: 'action',
        icon: <Activity className="size-4 text-muted" aria-hidden />,
        terms: 'system health control plane doctor',
        run: go(() => navigate({ to: '/system' })),
      },
    ]
    return [
      ...apps,
      ...spaces,
      ...actions.filter((action) =>
        action.key === 'action:system'
          ? data?.platformAdmin
          : action.key === 'action:machine'
            ? hasMachine
            : data?.platformAdmin || Object.values(data?.roles ?? {}).some((role) => role !== 'viewer'),
      ),
    ]
  }, [data, hasMachine, navigate, onClose, openDeploy, openMachine])

  const q = query.trim().toLowerCase()
  const results = q ? items.filter((i) => i.terms.includes(q)) : items

  return (
    <Dialog
      open={open}
      onClose={() => {
        setQuery('')
        onClose()
      }}
      className="relative z-[60]"
    >
      <DialogBackdrop transition className={backdropClass} />
      <div className="fixed inset-0 flex items-start justify-center p-4 sm:pt-[8vh]">
        <DialogPanel
          transition
          className="w-full max-w-xl overflow-hidden rounded-xl border border-line bg-card shadow-2xl transition duration-(--duration-base) ease-emphasized data-closed:scale-[.98] data-closed:opacity-0 data-leave:duration-(--duration-fast) data-leave:ease-exit"
        >
          <Combobox<Item | null> immediate onChange={(item) => item?.run()}>
            <div className="flex h-14 items-center gap-2 border-b border-line px-5">
              <Search className="size-4 shrink-0 text-muted" aria-hidden />
              <ComboboxInput
                autoFocus
                aria-label="Search apps, spaces and actions"
                placeholder="Jump to an app, space, or action…"
                displayValue={() => ''}
                onChange={(e) => setQuery(e.target.value)}
                className="w-full bg-transparent text-sm text-ink placeholder:text-muted focus:outline-none"
              />
              <kbd className="hidden rounded border border-line px-1.5 py-0.5 font-mono text-[10px] text-muted sm:inline">
                esc
              </kbd>
            </div>
            <ComboboxOptions static className="max-h-80 overflow-y-auto py-1">
              {results.length === 0 ? (
                <p className="px-5 py-6 text-center text-sm text-muted">Nothing matches “{query}”.</p>
              ) : (
                results.map((item) => (
                  <ComboboxOption
                    key={item.key}
                    value={item}
                    className="flex h-9 items-center gap-2.5 px-5 text-sm text-ink-soft data-focus:bg-accent-soft data-focus:text-ink"
                  >
                    <span className="flex size-4 shrink-0 items-center justify-center">{item.icon}</span>
                    <span className="truncate font-mono text-ink">{item.label}</span>
                    {item.hint && <span className="ml-auto shrink-0 text-xs text-muted">{item.hint}</span>}
                  </ComboboxOption>
                ))
              )}
            </ComboboxOptions>
          </Combobox>
        </DialogPanel>
      </div>
    </Dialog>
  )
}

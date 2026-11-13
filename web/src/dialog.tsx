// The console's one popup frame.
//
// Escape closes the topmost overlay — Headless UI owns that. An inline
// editor that wants Escape for itself ("cancel this edit") calls
// preventDefault on it; nothing else intercepts the key.
//
// Every dialog — a confirm, a form, a workbench with sidebars — is a Modal
// with the same backdrop, position, radius, shadow and motion, built from
// the same three bands: ModalHeader (icon, eyebrow, title, extra controls,
// an action portlet with the close button at the far right), ModalBody (the
// one scrolling region) and ModalFooter (buttons, right-aligned, an optional
// note on the left). Width and height are picked from short scales so two
// dialogs never differ by a few pixels for no reason.
//
// Drawers (the app panel, deploy, telemetry) are not Headless UI dialogs but
// wear the same header through PaneHeader, so a title row looks identical
// whether it sits in a popup or a side panel.
import { Dialog, DialogBackdrop, DialogPanel, DialogTitle, Transition } from '@headlessui/react'
import { X } from 'lucide-react'
import type { ElementType, ReactNode } from 'react'
import { ErrorNote, Eyebrow, IconButton, Toolbar } from './ui'

export type ModalSize = 'sm' | 'md' | 'lg' | 'xl'
export type ModalHeight = 'auto' | 'medium' | 'fill'

const widths: Record<ModalSize, string> = {
  sm: 'max-w-md', // confirmations
  md: 'max-w-lg', // forms
  lg: 'max-w-3xl', // settings with a sidebar
  xl: 'max-w-5xl', // workbenches: database, keys, buckets, requests
}

// Dialogs hang from the same top edge (8vh on desktop, the page padding on
// phones). Auto-height ones grow with their content up to the viewport;
// fixed-height ones keep their size whatever tab or view is showing, so the
// frame never jumps under the pointer.
const heights: Record<ModalHeight, string> = {
  auto: 'max-h-[calc(100dvh-2rem)] sm:max-h-[calc(92dvh-2rem)]',
  medium: 'h-[calc(100dvh-2rem)] sm:h-[min(40rem,calc(92dvh-2rem))]',
  fill: 'h-[calc(100dvh-2rem)] sm:h-[min(56rem,calc(92dvh-2rem))]',
}

export const backdropClass =
  'fixed inset-0 bg-scrim/60 transition-opacity duration-(--duration-base) ease-emphasized data-closed:opacity-0 data-leave:duration-(--duration-fast) data-leave:ease-exit'

export function Modal({
  open = true,
  onClose,
  size = 'md',
  height = 'auto',
  layer = 'z-50',
  children,
}: {
  open?: boolean
  /** Called on Escape and backdrop clicks. Guard it with `busy` yourself. */
  onClose: () => void
  size?: ModalSize
  height?: ModalHeight
  /** The palette sits above everything else; pass z-[60] there. */
  layer?: string
  children: ReactNode
}) {
  // Most popups are mounted when they are wanted — already open. `appear`
  // makes that first showing an enter like any other. Without it Headless UI
  // paints the panel as it is, decides only after that paint that it may
  // animate, hides the panel and fades it in: a popup that is there, gone
  // and there again.
  return (
    <Transition show={open} appear>
      <Dialog onClose={onClose} className={`relative ${layer}`}>
        <DialogBackdrop transition className={backdropClass} />
        <div className="fixed inset-0 flex items-start justify-center p-4 sm:pt-[8vh]">
          <DialogPanel
            transition
            className={`flex w-full flex-col overflow-hidden rounded-xl border border-line bg-card shadow-2xl transition duration-(--duration-base) ease-emphasized data-closed:scale-[.98] data-closed:opacity-0 data-leave:duration-(--duration-fast) data-leave:ease-exit ${widths[size]} ${heights[height]}`}
          >
            {children}
          </DialogPanel>
        </div>
      </Dialog>
    </Transition>
  )
}

type HeaderProps = {
  /** The kind of thing, above the title: "Database · acme". */
  eyebrow?: ReactNode
  title: ReactNode
  /** Extra controls that belong to the title (a model picker, a database
   *  switch). They sit between the title and the action portlet. */
  children?: ReactNode
  /** The action portlet, top right. Icon buttons, in the order of use. */
  actions?: ReactNode
  onClose?: () => void
  closeLabel?: string
  closeDisabled?: boolean
  /** Drawers want the big title; dialogs the compact one. */
  prominent?: boolean
}

function HeaderFrame({
  Title,
  eyebrow,
  title,
  children,
  actions,
  onClose,
  closeLabel = 'Close',
  closeDisabled = false,
  prominent = false,
}: HeaderProps & { Title: ElementType }) {
  return (
    <header className={`flex shrink-0 items-center gap-3 border-b border-line px-5 ${prominent ? 'py-4' : 'py-3'}`}>
      <div className="min-w-0 flex-1">
        {eyebrow && <Eyebrow className="truncate">{eyebrow}</Eyebrow>}
        <Title
          className={`truncate tracking-tight text-ink ${prominent ? 'font-mono text-xl font-bold' : 'text-sm font-semibold'}`}
        >
          {title}
        </Title>
      </div>
      {children && <div className="flex min-w-0 shrink items-center gap-2">{children}</div>}
      {(actions || onClose) && (
        <Toolbar className="-mr-2">
          {actions}
          {onClose && <IconButton icon={X} label={`${closeLabel} (Esc)`} onClick={onClose} disabled={closeDisabled} />}
        </Toolbar>
      )}
    </header>
  )
}

export function ModalHeader(props: HeaderProps) {
  return <HeaderFrame Title={DialogTitle} {...props} />
}

// PaneHeader is the same row for side panels, which are not Headless UI
// dialogs (DialogTitle needs one around it).
export function PaneHeader(props: HeaderProps) {
  return <HeaderFrame Title="h1" {...props} />
}

// ModalBody is the one region that scrolls. `padded={false}` hands the
// whole area to a split layout (sidebar + main) that pads itself.
export function ModalBody({
  children,
  padded = true,
  className = '',
}: {
  children: ReactNode
  padded?: boolean
  className?: string
}) {
  return (
    <div className={`min-h-0 flex-1 ${padded ? 'overflow-y-auto px-5 py-4' : 'flex flex-col'} ${className}`}>
      {children}
    </div>
  )
}

// ModalNotice sits between header and body for errors that concern the
// whole dialog. It takes no room when there is nothing to say.
export function ModalNotice({ message }: { message: string }) {
  if (!message) return null
  return (
    <div className="shrink-0 border-b border-line px-5 py-3">
      <ErrorNote message={message} />
    </div>
  )
}

export function ModalFooter({ children, note }: { children: ReactNode; note?: ReactNode }) {
  return (
    <footer className="flex shrink-0 flex-wrap items-center justify-end gap-2 border-t border-line px-5 py-3">
      {note && <p className="mr-auto min-w-0 flex-1 basis-48 text-xs leading-relaxed text-muted">{note}</p>}
      {children}
    </footer>
  )
}

// SplitPane is the workbench layout: a sidebar of things on the left, the
// chosen thing on the right. Both scroll on their own.
export function SplitPane({
  sidebar,
  children,
  sidebarWidth = 'w-40 sm:w-56',
}: {
  sidebar: ReactNode
  children: ReactNode
  sidebarWidth?: string
}) {
  return (
    <div className="flex min-h-0 flex-1">
      <aside className={`flex ${sidebarWidth} shrink-0 flex-col border-r border-line`}>{sidebar}</aside>
      <section className="flex min-w-0 flex-1 flex-col">{children}</section>
    </div>
  )
}

// Drawer is the side panel: the app, deploy and telemetry views slide in
// from the right over the fleet. It is a Headless UI dialog like every
// popup, so focus, Escape, nested dialogs and the page scroll lock are all
// handled the same way. `open` drives the slide; when the leave transition
// ends, `afterClose` fires — the route-bound drawers navigate away then, so
// the exit is seen rather than cut off by the unmount.
export function Drawer({
  open,
  onClose,
  afterClose,
  label,
  width = 'max-w-3xl',
  children,
}: {
  open: boolean
  onClose: () => void
  afterClose?: () => void
  label: string
  width?: string
  children: ReactNode
}) {
  return (
    <Transition show={open} appear afterLeave={afterClose}>
      <Dialog onClose={onClose} aria-label={label} className="relative z-50">
        <DialogBackdrop
          transition
          className="fixed inset-0 bg-scrim/50 transition-opacity duration-(--duration-panel) ease-emphasized data-closed:opacity-0 data-leave:duration-(--duration-panel-exit) data-leave:ease-exit"
        />
        <div className="fixed inset-0 flex justify-end">
          <DialogPanel
            transition
            className={`flex h-full w-full ${width} flex-col overflow-hidden border-l border-line bg-card shadow-[-24px_0_60px] shadow-scrim/50 transition-transform duration-(--duration-panel) ease-emphasized will-change-transform data-closed:translate-x-full data-leave:duration-(--duration-panel-exit) data-leave:ease-exit`}
          >
            {children}
          </DialogPanel>
        </div>
      </Dialog>
    </Transition>
  )
}

// DrawerBody is the one scrolling region of a drawer. Keyed by the caller
// per view (a tab), so each view starts at the top instead of inheriting
// the previous one's scroll offset. A view that fills the height (logs)
// lays out as a column and scrolls inside its own pane instead.
export function DrawerBody({ children, className = '' }: { children: ReactNode; className?: string }) {
  return (
    <div className={`flex min-h-0 flex-1 flex-col gap-5 overflow-y-auto overscroll-none px-5 py-5 ${className}`}>
      {children}
    </div>
  )
}

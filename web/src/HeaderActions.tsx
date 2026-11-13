import { createContext, useContext, useState, type ReactNode } from 'react'
import { createPortal } from 'react-dom'

// A page's own actions, shown in the console's top bar.
//
// Most pages keep their actions beside their title. A page that hands the
// whole window to one thing — the machine's embedded agent — has no title row
// to put them in, and spending one on four icons would cost the agent the
// height it is there for. Such a page renders <HeaderActions>, and the bar
// shows them left of its own icons for as long as the page is open.
const SlotContext = createContext<HTMLElement | null>(null)
const SetSlotContext = createContext<(slot: HTMLElement | null) => void>(() => {})

export function HeaderActionsProvider({ children }: { children: ReactNode }) {
  const [slot, setSlot] = useState<HTMLElement | null>(null)
  return (
    <SetSlotContext.Provider value={setSlot}>
      <SlotContext.Provider value={slot}>{children}</SlotContext.Provider>
    </SetSlotContext.Provider>
  )
}

/** Where the top bar shows the open page's actions; empty, it takes no room. */
export function HeaderActionsSlot() {
  const setSlot = useContext(SetSlotContext)
  return <div ref={setSlot} className="flex shrink-0 items-center gap-1 empty:hidden" />
}

/** What a page puts in the top bar: icon buttons, in the order of use. */
export function HeaderActions({ children }: { children: ReactNode }) {
  const slot = useContext(SlotContext)
  if (!slot) return null
  return createPortal(
    <>
      {children}
      {/* The page's actions end here; the console's own begin. */}
      <span className="mx-1 h-4 w-px bg-line" aria-hidden />
    </>,
    slot,
  )
}

import { useEffect, useRef, useState } from 'react'
import { LogOut, UserRound } from 'lucide-react'

/**
 * The account control in the header.
 *
 * Sign out is the one irreversible thing up there, and it sat next to Deploy
 * as a plain button of equal weight. Behind an icon it takes one more
 * deliberate click, and the header stops spending its width on the action
 * people use least.
 */
export function UserMenu({ onSignOut }: { onSignOut: () => void }) {
  const [open, setOpen] = useState(false)
  const ref = useRef<HTMLDivElement>(null)

  // A menu that only closes by re-clicking its own button is a menu people
  // leave open. Escape and a click anywhere else both dismiss it.
  useEffect(() => {
    if (!open) return
    const onDown = (e: MouseEvent) => {
      if (ref.current && !ref.current.contains(e.target as Node)) setOpen(false)
    }
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') setOpen(false)
    }
    document.addEventListener('mousedown', onDown)
    document.addEventListener('keydown', onKey)
    return () => {
      document.removeEventListener('mousedown', onDown)
      document.removeEventListener('keydown', onKey)
    }
  }, [open])

  return (
    <div className="relative" ref={ref}>
      <button
        type="button"
        onClick={() => setOpen(!open)}
        title="Account"
        aria-label="Account"
        aria-haspopup="menu"
        aria-expanded={open}
        className="flex items-center rounded-md p-1.5 text-stone hover:bg-line/60 hover:text-ink focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-hay-deep"
      >
        <UserRound className="size-5" aria-hidden />
      </button>

      {open && (
        <div
          role="menu"
          className="absolute right-0 z-50 mt-1 w-44 overflow-hidden rounded-md border border-line bg-card py-1 shadow-lg"
        >
          <button
            type="button"
            role="menuitem"
            onClick={() => {
              setOpen(false)
              onSignOut()
            }}
            className="flex w-full items-center gap-2 px-3 py-2 text-left text-sm text-ink-soft hover:bg-hay-wash hover:text-ink"
          >
            <LogOut className="size-4" aria-hidden /> Sign out
          </button>
        </div>
      )}
    </div>
  )
}

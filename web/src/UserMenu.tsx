import { Menu, MenuButton, MenuItem, MenuItems } from '@headlessui/react'
import { LogOut, UserRound } from 'lucide-react'

/**
 * The account control in the header.
 *
 * Sign out is the one irreversible thing up there, and it sat next to Deploy
 * as a plain button of equal weight. Behind an icon it takes one more
 * deliberate click, and the header stops spending its width on the action
 * people use least. Dismissal, Escape and keyboard navigation come from
 * Headless UI.
 */
export function UserMenu({ onSignOut }: { onSignOut: () => void }) {
  return (
    <Menu>
      <MenuButton
        title="Account"
        aria-label="Account"
        className="flex items-center rounded-md p-1.5 text-muted hover:bg-line/60 hover:text-ink focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-accent-strong data-open:bg-line/60 data-open:text-ink"
      >
        <UserRound className="size-5" aria-hidden />
      </MenuButton>
      <MenuItems
        anchor="bottom end"
        className="z-50 mt-1 w-44 overflow-hidden rounded-md border border-line bg-card py-1 shadow-lg focus:outline-none"
      >
        <MenuItem>
          <button
            type="button"
            onClick={onSignOut}
            className="flex w-full items-center gap-2 px-3 py-2 text-left text-sm text-ink-soft data-focus:bg-accent-soft data-focus:text-ink"
          >
            <LogOut className="size-4" aria-hidden /> Sign out
          </button>
        </MenuItem>
      </MenuItems>
    </Menu>
  )
}

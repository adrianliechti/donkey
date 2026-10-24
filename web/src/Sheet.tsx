import type { ReactNode } from 'react'
import { useScrolledPast } from './hooks'

/**
 * The frame of an app's or a service's page: the overviews' dotted surface,
 * and on it one sheet — the tab row along its top edge, the tab below. The
 * sheet sits on the same 24px gutter as the logo and the overview tiles, so
 * moving from a space into one of its apps keeps every edge where it was.
 *
 * The tab row sticks under the breadcrumb (top-13). A sticky child must not
 * sit in an overflow-hidden frame, so the sheet does not clip: the row
 * rounds its own top corners, inside the sheet's border — until it sticks,
 * 24px down (the gutter above the sheet), when it squares them so nothing
 * shows through behind.
 *
 * One scroller at a time: a document tab (an overview, settings) scrolls
 * with the page, while a workbench (the log pane, a file browser, a service
 * console) asks to `fill` — the sheet then takes exactly the window under
 * the breadcrumb, the page stands still, and only the workbench's own panes
 * scroll. Its body must be a flex column that fills (min-h-0 flex-1).
 */
export function Sheet({ tabs, fill = false, children }: { tabs?: ReactNode; fill?: boolean; children: ReactNode }) {
  const stuck = useScrolledPast(24)
  return (
    <div className={`dotgrid flex flex-col p-6 ${fill ? 'h-[calc(100dvh-52px)] min-h-[36rem]' : 'flex-1'}`}>
      <div className="flex min-h-0 flex-1 flex-col rounded-xl border border-line bg-card shadow-sm">
        {tabs && (
          <header
            className={`sticky top-13 z-20 border-b border-line bg-card/95 backdrop-blur ${
              stuck ? '' : 'rounded-t-[calc(var(--radius-xl)-1px)]'
            }`}
          >
            {tabs}
          </header>
        )}
        {children}
      </div>
    </div>
  )
}

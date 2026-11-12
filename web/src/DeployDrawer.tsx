import { useCallback, useState, type ReactNode } from 'react'
import { useNavigate } from '@tanstack/react-router'
import { DeployView } from './DeployView'
import { type DeployTarget, OpenDeploy } from './deploy'
import { Drawer, DrawerBody, PaneHeader } from './dialog'

/**
 * Deploying is one drawer for the whole console, opened from wherever you
 * are — a space's ⋯, an app's header, ⌘K — and sliding over that page, so
 * the rest stays in view while a new version comes up.
 */
export function DeployProvider({ children }: { children: ReactNode }) {
  const navigate = useNavigate()
  const [target, setTarget] = useState<DeployTarget | null>(null)
  const [open, setOpen] = useState(false)
  // A fresh deploy view per opening: the last one's files and plan are gone.
  const [round, setRound] = useState(0)

  const openDeploy = useCallback((next: DeployTarget = {}) => {
    setTarget(next)
    setRound((n) => n + 1)
    setOpen(true)
  }, [])

  const space = target?.space ?? ''
  const name = target?.name ?? ''
  return (
    <OpenDeploy.Provider value={openDeploy}>
      {children}
      <Drawer open={open} onClose={() => setOpen(false)} label="Deploy an app" width="max-w-4xl">
        <PaneHeader
          prominent
          eyebrow={name ? `Redeploy · ${space}` : space ? `Deploy into ${space}` : 'Deploy'}
          title={name || 'New app'}
          onClose={() => setOpen(false)}
          closeLabel="Close deploy"
        />
        <DrawerBody>
          <DeployView
            key={round}
            space={space}
            name={name}
            onDeployed={(s, n) => {
              setOpen(false)
              navigate({ to: '/app/$space/$name/{-$tab}', params: { space: s, name: n, tab: undefined } })
            }}
          />
        </DrawerBody>
      </Drawer>
    </OpenDeploy.Provider>
  )
}

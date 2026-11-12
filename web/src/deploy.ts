import { createContext, useContext } from 'react'

// What a deploy starts from: nothing (the space is chosen in the review), a
// space to deploy into, or an app to redeploy.
export type DeployTarget = { space?: string; name?: string }

// Opens the console's one deploy drawer (see DeployDrawer) over the page.
export const OpenDeploy = createContext<(target?: DeployTarget) => void>(() => {})

export function useDeploy() {
  return useContext(OpenDeploy)
}

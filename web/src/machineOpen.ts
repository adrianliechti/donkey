import { createContext, useContext } from 'react'
import type { MachineSource, OpenFolder } from './api'

// What a new folder of a machine may be called — the rule the machine
// applies, so a name is refused while it is typed rather than after the click.
export const folderNameRE = /^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$/

/** "an hour", "90 minutes", "2 hours": how long a machine may go unused. */
export function idleText(minutes: number): string {
  if (minutes === 60) return 'an hour'
  if (minutes % 60 === 0) return `${minutes / 60} hours`
  return `${minutes} minutes`
}

// What the dialog that asks for a folder hands to the Machine page when it
// sends its visitor there: the folder to open — one the machine has, a new
// one, or one made from an app's version — or 'look', to start a stopped
// machine only to see what is on its disk.
//
// It travels in memory, never in the URL. Opening a folder can start a
// machine, and it ends what the machine's agent is doing — nothing a link
// should be able to do to whoever follows it. The page that receives it
// takes it once.
export type Handed = OpenFolder | 'look'

let handed: Handed | null = null

export function handToMachine(what: Handed) {
  handed = what
}

export function handedToMachine(): Handed | null {
  return handed
}

export function clearHandedToMachine() {
  handed = null
}

// Goes to one's machine from anywhere (see OpenMachineProvider): straight to
// the agent where a folder is open, and otherwise by asking which folder
// first, over the page one is on. With an app's version, it asks for the
// folder that version should become.
export const OpenMachine = createContext<(source?: MachineSource) => void>(() => {})

export function useOpenMachine() {
  return useContext(OpenMachine)
}

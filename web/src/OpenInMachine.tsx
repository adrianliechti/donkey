import { useQuery } from '@tanstack/react-query'
import { FolderCode } from 'lucide-react'
import { useOpenMachine } from './machineOpen'
import { machineQuery } from './queries'
import { IconButton } from './ui'

/**
 * OpenInMachine offers one version of an app's source to the person's own
 * machine. It opens the dialog that asks for a folder (MachineDialog) on
 * that version, over the page it sits on — so changing one's mind leaves
 * one where one was.
 *
 * It is there only for people who may have a machine: nobody else is shown a
 * way to one.
 */
export function OpenInMachine({
  space,
  app,
  commit,
  size = 'md',
  label = 'Open this version in your machine',
}: {
  space: string
  app: string
  commit: string
  size?: 'sm' | 'md'
  label?: string
}) {
  const { data: machine } = useQuery(machineQuery)
  const openMachine = useOpenMachine()
  if (!machine?.enabled || !machine.allowed) return null
  return <IconButton icon={FolderCode} size={size} label={label} onClick={() => openMachine({ space, app, commit })} />
}

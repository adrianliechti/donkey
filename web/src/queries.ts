// The console's data layer, on TanStack Query: every read is cached under a
// key, so a remounted panel shows what it knew instantly and refreshes in
// the background — no skeleton flash for data the console already had.
// Reads are queryOptions factories, so route loaders can warm the cache
// (`ensureQueryData`) before a view renders. Writes stay plain api.* calls;
// they just invalidate the keys they touch.
import {
  queryOptions,
  useQuery,
  useQueryClient,
  type UseQueryResult,
} from '@tanstack/react-query'
import { api } from './api'
import { errText } from './ui'

export const fleetQuery = queryOptions({
  queryKey: ['fleet'],
  queryFn: async () => {
    const [apps, spaces] = await Promise.all([api.apps(), api.spaces()])
    return { apps: apps.apps, spaces: spaces.spaces }
  },
  refetchInterval: 10_000,
})

export const statusQuery = (space: string, app: string) =>
  queryOptions({
    queryKey: ['status', space, app],
    queryFn: () => api.status(space, app),
    refetchInterval: 10_000,
  })

// Doctor probes everything (build engine, registry, gateway, assistant) and
// can take up to ~15s — poll gently; the cache keeps the last report
// visible while a new one is on its way.
export const doctorQuery = queryOptions({
  queryKey: ['doctor'],
  queryFn: () => api.doctor(),
  refetchInterval: 60_000,
})

export const appServicesQuery = (space: string, app: string) =>
  queryOptions({
    queryKey: ['app-services', space, app],
    queryFn: async () => (await api.addons(space, app)).addons ?? [],
  })

export const spaceServicesQuery = (space: string) =>
  queryOptions({
    queryKey: ['space-services', space],
    queryFn: async () => (await api.spaceAddons(space)).addons ?? [],
  })

// The catalog changes with a server release, not at runtime.
export const catalogQuery = queryOptions({
  queryKey: ['catalog'],
  queryFn: async () => (await api.addonTypes()).types ?? [],
  staleTime: 60 * 60 * 1000,
})

// present maps a query result into the shape the console's views grew up
// with: data-or-null, a plain error sentence, when it was last fresh,
// reload. Pure — the useQuery call stays visible at every call site.
function present<T>(q: UseQueryResult<T>) {
  return {
    data: q.data ?? null,
    error: q.error ? errText(q.error, 'Loading did not work.') : '',
    updatedAt: q.dataUpdatedAt || null,
    reload: () => {
      q.refetch()
    },
  }
}

export function useFleet() {
  return present(useQuery(fleetQuery))
}

export function useAppStatus(space: string, app: string) {
  return present(useQuery(statusQuery(space, app)))
}

export function useDoctor() {
  return present(useQuery(doctorQuery))
}

export function useAppServices(space: string, app: string) {
  return present(useQuery(appServicesQuery(space, app)))
}

export function useSpaceServices(space: string) {
  return present(useQuery(spaceServicesQuery(space)))
}

export function useCatalog() {
  return present(useQuery(catalogQuery))
}

// One service change shows up in three places — the space strip, the app's
// card, and the fleet chips. Invalidating them together is what keeps the
// console from needing reload callbacks threaded through every component.
export function useInvalidateServices() {
  const qc = useQueryClient()
  return (space: string) => {
    qc.invalidateQueries({ queryKey: spaceServicesQuery(space).queryKey })
    qc.invalidateQueries({ queryKey: ['app-services', space] })
    qc.invalidateQueries({ queryKey: fleetQuery.queryKey })
  }
}

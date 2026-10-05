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
import { api, type ResourceRange, type TrafficRange, type TrafficSource } from './api'
import { errText } from './ui'

export const fleetQuery = queryOptions({
  queryKey: ['fleet'],
  queryFn: async () => {
    const [apps, spaces] = await Promise.all([api.apps(), api.spaces()])
    return { apps: apps.apps, ...spaces }
  },
  refetchInterval: 10_000,
})

export const statusQuery = (space: string, app: string) =>
  queryOptions({
    queryKey: ['status', space, app],
    queryFn: () => api.status(space, app),
    refetchInterval: 10_000,
  })

export const buildsQuery = (space: string, app: string) =>
  queryOptions({
    queryKey: ['builds', space, app],
    queryFn: () => api.builds(space, app),
  })

export const buildQuery = (space: string, app: string, sequence: number) =>
  queryOptions({
    queryKey: ['build', space, app, sequence],
    queryFn: () => api.build(space, app, sequence),
    staleTime: Infinity, // a stored build never changes
  })

export const trafficQuery = (space: string, app = '', source: TrafficSource = 'edge', range: TrafficRange = '30d') =>
  queryOptions({
    queryKey: ['traffic', space, app, source, range],
    queryFn: () => api.traffic(space, app, source, range),
    refetchInterval: 30_000,
  })

export const resourcesQuery = (space: string, app: string, range: ResourceRange = '4h') =>
  queryOptions({
    queryKey: ['resources', space, app, range],
    queryFn: () => api.resources(space, app, range),
    refetchInterval: 30_000,
  })

// Doctor probes everything (build engine, registry, gateway, assistant) and
// can take up to ~15s — poll gently; the cache keeps the last report
// visible while a new one is on its way.
export const doctorQuery = queryOptions({
  queryKey: ['doctor'],
  queryFn: () => api.doctor(),
  refetchInterval: 60_000,
})

// The assistant's reading of a failing app. Each run is a model call over
// the app's real logs, so it is cached per deployed image: opening Logs again
// does not ask again, a new deploy does, and "Check again" is the explicit
// re-run.
export const diagnosisQuery = (space: string, app: string, image: string) =>
  queryOptions({
    queryKey: ['diagnosis', space, app, image],
    queryFn: () => api.diagnose(space, app),
    staleTime: Infinity,
    gcTime: 30 * 60_000,
    retry: false,
    refetchOnWindowFocus: false,
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

// Providers refresh their catalogs, and admins edit availability per space.
export const catalogQuery = queryOptions({
  queryKey: ['catalog'],
  queryFn: async () => (await api.addonTypes()).types ?? [],
  staleTime: 30 * 1000,
})

export const spaceCatalogQuery = (space: string) => queryOptions({
  queryKey: ['catalog', space],
  queryFn: async () => (await api.addonTypes(space)).types ?? [],
  staleTime: 30 * 1000,
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

export function useAppBuilds(space: string, app: string) {
  return present(useQuery(buildsQuery(space, app)))
}

export function useAppBuild(space: string, app: string, sequence: number | null) {
  return present(useQuery({ ...buildQuery(space, app, sequence ?? 0), enabled: sequence !== null }))
}

// enabled=false only reads what an earlier run left in the cache.
export function useDiagnosis(space: string, app: string, image: string, enabled = true) {
  const q = useQuery({ ...diagnosisQuery(space, app, image), enabled })
  return { ...present(q), fetching: q.isFetching }
}

export function useTraffic(space: string, app = '', source: TrafficSource = 'edge', range: TrafficRange = '30d') {
  return present(useQuery(trafficQuery(space, app, source, range)))
}

export function useResources(space: string, app: string, range: ResourceRange = '4h') {
  return present(useQuery(resourcesQuery(space, app, range)))
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

export function useCatalog(space?: string) {
  return present(useQuery(space ? spaceCatalogQuery(space) : catalogQuery))
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

// The console's data layer, on TanStack Query: every read is cached under a
// key, so a remounted panel shows what it knew instantly and refreshes in
// the background — no skeleton flash for data the console already had.
// Reads are queryOptions factories, so route loaders can warm the cache
// (`ensureQueryData`) before a view renders. Writes are mutations that
// invalidate the keys they touch (older views still call api.* directly and
// invalidate by hand).
import { useState } from 'react'
import {
  keepPreviousData,
  queryOptions,
  useMutation,
  useQuery,
  useQueryClient,
  type UseQueryResult,
} from '@tanstack/react-query'
import {
  type AddAddonRequest,
  type AnalyticsFilter,
  api,
  type ResourceRange,
  type SpaceConfig,
  type TrafficRange,
  type TrafficSource,
} from './api'
import { errText } from './format'

export const fleetQuery = queryOptions({
  queryKey: ['fleet'],
  queryFn: () => api.fleet(),
  refetchInterval: 10_000,
})

// The deploy board: watched closely while something is under way, lazily
// otherwise — a deploy started from the CLI shows up within seconds either way.
export const deploysQuery = queryOptions({
  queryKey: ['deploys'],
  queryFn: () => api.deploys(),
  refetchInterval: (query) => ((query.state.data ?? []).some((d) => !d.finishedAt) ? 2_000 : 10_000),
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

// The source history. A commit's files never change, so trees, files and
// diffs are cached for good; the list and a commit's builds do grow.
export const commitsQuery = (space: string, app: string) =>
  queryOptions({
    queryKey: ['commits', space, app],
    queryFn: () => api.commits(space, app),
  })

export const commitQuery = (space: string, app: string, id: string) =>
  queryOptions({
    queryKey: ['commit', space, app, id],
    queryFn: () => api.commit(space, app, id),
  })

export const commitDiffQuery = (space: string, app: string, id: string, path: string) =>
  queryOptions({
    queryKey: ['commit-diff', space, app, id, path],
    queryFn: () => api.commitDiff(space, app, id, path),
    staleTime: Infinity,
  })

export const commitTreeQuery = (space: string, app: string, id: string) =>
  queryOptions({
    queryKey: ['commit-tree', space, app, id],
    queryFn: () => api.commitTree(space, app, id),
    staleTime: Infinity,
  })

export const commitFileQuery = (space: string, app: string, id: string, path: string) =>
  queryOptions({
    queryKey: ['commit-file', space, app, id, path],
    queryFn: () => api.commitFile(space, app, id, path),
    staleTime: Infinity,
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

// A new range or filter keeps the previous report on screen (dimmed by the
// panel) until the new one arrives, so the dashboard never blanks out.
export const analyticsQuery = (space: string, app: string, range: TrafficRange, filters: AnalyticsFilter[]) =>
  queryOptions({
    queryKey: ['analytics', space, app, range, filters],
    queryFn: () => api.analytics(space, app, range, filters),
    refetchInterval: 60_000,
    placeholderData: keepPreviousData,
  })

export const analyticsLiveQuery = (space: string, app: string) =>
  queryOptions({
    queryKey: ['analytics-live', space, app],
    queryFn: () => api.analyticsLive(space, app),
    refetchInterval: 15_000,
  })

// One folder of the app's disk. An app without a disk answers with an error
// that says so — the files tab reads it as "nothing to show", not a failure.
export const filesQuery = (space: string, app: string, path: string) =>
  queryOptions({
    queryKey: ['files', space, app, path],
    queryFn: () => api.files(space, app, path),
    retry: false,
  })

export const networkQuery = (space: string, app: string) =>
  queryOptions({
    queryKey: ['network', space, app],
    queryFn: () => api.network(space, app),
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

// A service's variables with their values — secrets, so never kept around
// longer than the page that shows them.
export const serviceBindingQuery = (space: string, service: string) =>
  queryOptions({
    queryKey: ['service-binding', space, service],
    queryFn: () => api.serviceBinding(space, service),
    gcTime: 0,
  })

// Providers refresh their catalogs, and admins edit availability per space.
export const catalogQuery = queryOptions({
  queryKey: ['catalog'],
  queryFn: async () => (await api.addonTypes()).types ?? [],
  staleTime: 30 * 1000,
})

export const spaceCatalogQuery = (space: string) =>
  queryOptions({
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

// What is being deployed, by app ("space/app"). When a deploy finishes, the
// fleet and that app's status are refreshed at once rather than at their next
// poll, so a placeholder turns into the app without a gap.
export function useDeploys() {
  const qc = useQueryClient()
  const q = useQuery(deploysQuery)
  const running = (q.data ?? [])
    .filter((d) => !d.finishedAt)
    .map((d) => `${d.space}/${d.app}`)
    .join(',')
  const [seen, setSeen] = useState(running)
  if (seen !== running) {
    setSeen(running)
    const done = seen.split(',').filter((key) => key && !running.split(',').includes(key))
    if (done.length > 0) {
      qc.invalidateQueries({ queryKey: fleetQuery.queryKey })
      for (const key of done) {
        const [space, app] = key.split('/')
        qc.invalidateQueries({ queryKey: statusQuery(space, app).queryKey })
      }
    }
  }
  const byApp = new Map((q.data ?? []).map((d) => [`${d.space}/${d.app}`, d]))
  return {
    deploys: q.data ?? [],
    byApp,
    error: q.error ? errText(q.error, 'Could not load recent deploys.') : '',
    loading: q.isPending,
  }
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

export function useCommits(space: string, app: string) {
  return present(useQuery(commitsQuery(space, app)))
}

export function useCommit(space: string, app: string, id: string | null) {
  return present(useQuery({ ...commitQuery(space, app, id ?? ''), enabled: id !== null }))
}

export function useCommitDiff(space: string, app: string, id: string, path: string, enabled = true) {
  return present(useQuery({ ...commitDiffQuery(space, app, id, path), enabled }))
}

export function useCommitTree(space: string, app: string, id: string) {
  return present(useQuery(commitTreeQuery(space, app, id)))
}

export function useCommitFile(space: string, app: string, id: string, path: string | null) {
  return present(useQuery({ ...commitFileQuery(space, app, id, path ?? ''), enabled: path !== null }))
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

export function useAnalytics(space: string, app: string, range: TrafficRange, filters: AnalyticsFilter[]) {
  const q = useQuery(analyticsQuery(space, app, range, filters))
  return { ...present(q), stale: q.isPlaceholderData }
}

export function useAnalyticsLive(space: string, app: string, enabled = true) {
  return present(useQuery({ ...analyticsLiveQuery(space, app), enabled }))
}

export function useNetwork(space: string, app: string, enabled = true) {
  return present(useQuery({ ...networkQuery(space, app), enabled }))
}

export function useDoctor() {
  return present(useQuery(doctorQuery))
}

export function useAppServices(space: string, app: string) {
  return present(useQuery(appServicesQuery(space, app)))
}

export function useSpaceServices(space: string, enabled = true) {
  return present(useQuery({ ...spaceServicesQuery(space), enabled }))
}

export function useServiceBinding(space: string, service: string) {
  return present(useQuery(serviceBindingQuery(space, service)))
}

export function useCatalog(space?: string, enabled = true) {
  return present(useQuery({ ...(space ? spaceCatalogQuery(space) : catalogQuery), enabled }))
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

// A mutation's error in the console's one voice, or '' while there is none.
export function mutationError(m: { error: unknown }, fallback: string) {
  return m.error ? errText(m.error, fallback) : ''
}

export function useCreateSpace() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: ({ name, settings }: { name: string; settings: SpaceConfig }) => api.createSpace(name, settings),
    onSuccess: () => qc.invalidateQueries({ queryKey: fleetQuery.queryKey }),
  })
}

export function useDeleteSpace(space: string) {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: () => api.deleteSpace(space),
    onSuccess: () => {
      qc.removeQueries({ queryKey: spaceServicesQuery(space).queryKey })
      return qc.invalidateQueries({ queryKey: fleetQuery.queryKey })
    },
  })
}

export function useAddService(space: string) {
  const invalidate = useInvalidateServices()
  return useMutation({
    mutationFn: (req: AddAddonRequest) => api.addSpaceAddon(space, req),
    onSuccess: () => invalidate(space),
  })
}

// Connecting a service to an app (or taking it away) restarts the app with
// the service's variables in or out.
export function useConnectService(space: string) {
  const invalidate = useInvalidateServices()
  return useMutation({
    mutationFn: ({ app, service, connect }: { app: string; service: string; connect: boolean }) =>
      connect ? api.addAddon(space, app, { name: service }) : api.removeAddon(space, app, service),
    onSuccess: () => invalidate(space),
  })
}

export function useRemoveService(space: string) {
  const invalidate = useInvalidateServices()
  return useMutation({
    mutationFn: ({ name, keepData, orphan }: { name: string; keepData: boolean; orphan: boolean }) =>
      api.removeSpaceAddon(space, name, keepData, orphan),
    onSuccess: () => invalidate(space),
  })
}

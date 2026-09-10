// Typed client for the donkey-server API. The session is the gateway's OIDC
// cookie and rides along on every request by itself; a 401 means it expired,
// and the registered handler reloads the page so the gateway signs in again.

/** How an app is reached: open to anyone, behind the login, or cluster-only. */
export type Exposure = 'public' | 'protected' | 'internal'
export type AppMode = 'web' | 'api'
export type CorsPolicy = '' | 'allowAll'

export interface AppSummary {
  name: string
  space: string
  healthy: boolean
  ready: string
  image: string
  url: string
  restarts?: number
  startedAt?: string
  exposure?: Exposure
  mode?: AppMode
  hasStorage?: boolean
  addons?: string[]
}

export interface AppStatus {
  found: boolean
  healthy: boolean
  ready: string
  image: string
  url: string
  pods: string[]
  restarts?: number
  startedAt?: string
  /** True when the app owns a disk — the only case where files exist to browse. */
  hasStorage?: boolean
  exposure?: Exposure
  mode?: AppMode
  port?: number
  endpoint?: EndpointStatus
}

export type EndpointState = 'ready' | 'issuing' | 'propagating' | 'error' | 'unknown' | 'unmanaged'

export interface CertificateStatus {
  state: EndpointState
  mode: 'acme' | 'ca' | 'cert-manager'
  issuer?: string
  reason?: string
  message?: string
  expiresAt?: string
  renewalTime?: string
  renewing?: boolean
}

export interface DNSStatus {
  state: EndpointState
  /** True once external-dns has read the current DNSEndpoint generation. */
  observed: boolean
  recordType?: 'A' | 'AAAA' | 'CNAME'
  expected?: string[]
  message?: string
}

export interface EndpointStatus {
  host: string
  certificate: CertificateStatus
  dns: DNSStatus
}

export type TrafficSource = 'edge' | 'app'
export type TrafficRange = '48h' | '7d' | '30d'
export type ResourceRange = '1h' | '4h' | '48h' | '7d' | '30d'

export interface TrafficTotals {
  requests: number
  successful: number
  clientErrors: number
  serverErrors: number
  otherResponses?: number
  averageLatencyMs: number
  p50LatencyMs: number
  p95LatencyMs: number
  views: number
  uniqueVisitors: number
  identifiedViews: number
  visitorCoveragePercent: number
}

export interface TrafficPoint extends TrafficTotals {
  bucket: string
}

export interface TrafficReport {
  space: string
  app?: string
  source: TrafficSource
  range: TrafficRange
  granularity: 'hour' | 'day'
  from: string
  to: string
  totals: TrafficTotals
  series: TrafficPoint[]
  definitions: {
    success: string
    view: string
    uniqueVisitors: string
  }
}

export interface ResourceCurrent {
  sampledAt: string
  fresh: boolean
  instances: number
  cpuCores: number
  memoryBytes: number
}

export interface ResourcePoint {
  bucket: string
  samples: number
  averageCpuCores: number
  peakCpuCores: number
  averageMemoryBytes: number
  peakMemoryBytes: number
}

export interface ResourceReport {
  space: string
  app: string
  range: ResourceRange
  granularity: 'minute' | '5m' | 'hour' | 'day'
  from: string
  to: string
  available: boolean
  current?: ResourceCurrent
  series: ResourcePoint[]
}

/** Who may open a login-protected app; empty lists mean "everyone signed in". */
export interface AccessConfig {
  users?: string[]
  groups?: string[]
  roles?: string[]
}

export interface SpaceConfig {
  admins: AccessConfig
  developers: AccessConfig
  domains: string[]
}

export interface PlatformConfig {
  admins: AccessConfig
}

export interface SpaceList {
  spaces: string[]
  roles: Record<string, 'admin' | 'developer'>
  domains: Record<string, string[]>
  platformAdmin: boolean
  domain: string
}

export interface RouteEntry {
  host: string
  path: string
}

export interface NetworkInfo {
	fqdn: string
	domains: string[]
  mode: AppMode
  exposure: Exposure
  /** Allow browser code on any origin to call the app through the gateway. */
  cors: CorsPolicy
  url?: string
  /** The generated <app>.<space>.<domain> address — the input placeholder. */
  defaultHost: string
  /** Set when the app answers somewhere else: a name of its own under the
   * space, "@" for the space's own address, or a full hostname. */
  host?: string
  port?: number
  routes?: RouteEntry[]
  access?: AccessConfig | null
}

export interface AddonEntry {
  name: string
  keys: string[]
}

/** One service of a space, and the apps connected to it. */
export interface SpaceAddonEntry {
  name: string
  keys: string[]
  attachedTo: string[]
}

/** One catalog entry: a service type donkey can provide, with its defaults
 * and the env variable names connected apps will see. */
export interface AddonType {
  /** The lowercase id the API speaks. */
  type: string
  /** The product's own name for display (PostgreSQL, RustFS, OpenAI). */
  title: string
  version: string
  storage: string
  description?: string
  keys?: string[]
}

export interface AddAddonRequest {
  /** Omitted when connecting to a service the space already has. */
  type?: string
  name?: string
  version?: string
  storage?: string
  external?: boolean
  url?: string
}

export interface FixAction {
  type: string
  path?: string
  newContent?: string
  key?: string
  value?: string
  explanation: string
  /** Unified diff from the file's current content to newContent, computed by the server. */
  diff?: string
}

export interface Diagnosis {
  diagnosis?: string
  proposals?: FixAction[]
  pods?: string[]
  events?: string[]
  logs?: string
  assistantError?: string
}

export interface FileEntry {
  name: string
  size: number
  mode: string
  modTime: string
  isDir: boolean
  isLink?: boolean
}

export interface DiskFolder {
  name: string
  path: string
}

export interface FileListing {
  path: string
  entries: FileEntry[]
  truncated: boolean
  folders?: DiskFolder[]
}

export interface DeployResult {
  apps?: DeployResult[]
  ok: boolean
  app: string
  space?: string
  imageRef?: string
  url?: string
  error?: string
  diagnosis?: string
  proposals?: FixAction[]
}

/** info: worth knowing. warn: the app will run and still not work.
 *  block: the deploy would refuse this. */
export type FindingLevel = 'info' | 'warn' | 'block'

/** One thing donkey noticed about an app. The server writes the words; the
 *  code is stable, so a view can render a finding its own way instead. */
export interface Finding {
  level: FindingLevel
  code: string
  title: string
  detail?: string
  fix?: string
  file?: string
}

/** Where the port came from — a review screen would rather explain a port
 *  than ask for one. */
export type PortSource = 'config' | 'code' | 'env' | 'guess'

export interface PlanApp {
  host: string
  domains: string[]
  name: string
  space: string
  port: number
  portSource: PortSource
  portReason?: string
  healthcheck?: string
  storage: string[]
  storageSize?: string
  exposure: Exposure
  mode: AppMode
  cors: CorsPolicy
  url?: string
}

export interface PlanBuild {
  summary: string
  strategy: string
  runtimes?: PlanSignal[]
  frameworks?: PlanSignal[]
  versions?: PlanSignal[]
  packageManagers?: PlanSignal[]
  baseImages?: string[]
  systemPackages?: PlanSystemPackage[]
}

export interface PlanSignal {
  value: string
  evidence?: string
}

export interface PlanSystemPackage {
  dependency: string
  build?: string[]
  runtime?: string[]
}

export interface PlanVolume {
  path: string
  reason?: string
  selected: boolean
}

export interface PlanService {
  type: string
  name: string
  reason?: string
  /** The space already runs one under this name — deploying connects to it. */
  exists: boolean
  /** False for a type donkey has no recipe for: reported, never offered. */
  known: boolean
  selected: boolean
}

/** A plan is a deploy read out loud before it happens: what the app is, how
 *  it will be built, what it needs, and what will break. */
export interface Plan {
  id: string
  app: PlanApp
  build: PlanBuild
  findings: Finding[]
  root?: string
  hasConfig: boolean
  appType?: string
  notes?: string
  volumes: PlanVolume[]
  services: PlanService[]
  skipped?: string[]
  /** The donkey.yaml this plan amounts to — a browser deploy has no folder to
   *  write it back to, so it is here to copy into the project. */
  config: string
  ready: boolean
  expiresAt: string
}

/** What the review screen decided. Every field is authoritative: an empty
 *  storage list means "no disk", not "you decide". */
export interface PlanEdit {
  host: string
  name: string
  space: string
  port: number
  healthcheck: string
  storage: string[]
  services: string[]
  exposure: Exposure
  mode: AppMode
  cors: CorsPolicy
}

export interface NetworkOptions {
  mode: AppMode
  exposure: Exposure
  host: string
  cors: CorsPolicy
}

// editFrom is the plan as an answer sheet: the server already decided every
// value, so the screen starts from its decisions rather than from blanks.
export function planEdit(plan: Plan): PlanEdit {
  return {
    name: plan.app.name,
    space: plan.app.space,
  host: plan.app.host ?? '',
    port: plan.app.port,
    healthcheck: plan.app.healthcheck ?? '',
    storage: [...plan.app.storage],
    services: plan.services.filter((s) => s.selected).map((s) => s.name),
    exposure: plan.app.exposure,
    mode: plan.app.mode,
    cors: plan.app.cors,
  }
}

export type DeployStage = 'detecting' | 'building' | 'releasing'

export interface SourceChange {
  path: string
  diff: string
}

/** Structured facts that change while a release is running. The regular
 * log remains the complete human-readable account. */
export interface DeployUpdate {
  app?: string
  result?: DeployResult
  stage?: DeployStage
  baseImages?: string[]
  dockerfile?: string
  dockerignore?: string
  changes?: SourceChange[]
}

export interface ProjectPlan {
  id: string
  apps: Plan[]
  connections: { app: string; target: string; env?: string; value?: string; reason: string }[]
  changes: { file: string; diff: string }[]
  findings: Finding[]
  ready: boolean
}

/** One versioned deploy record from the app's retained build history. */
export interface AppRelease {
  startedAt: string
  finishedAt: string
  plan?: Plan
  update: DeployUpdate
  lines: string[]
  buildLog?: string
  dockerfile?: string
  dockerignore?: string
  changes?: SourceChange[]
  result: DeployResult
  truncated?: boolean
}

/** One line of an app's build history, newest first. */
export interface BuildSummary {
  sequence: number
  created: string
  image?: string
  ok: boolean
}

export interface DoctorCheck {
  name: string
  ok: boolean
  detail?: string
}

export interface DoctorReport {
  checks: DoctorCheck[]
  domain: string
}

export interface RedisKey {
  id: string
  name: string
}

export interface RedisValue {
  type: string
  ttl: number
  size: number
  value: unknown
  editable: boolean
  truncated: boolean
}

/** One database of a browsed postgres service. */
export interface DbDatabase {
  name: string
  size: number
}

/** One table of a browsed database; rows is the planner's estimate. */
export interface DbTable {
  schema: string
  name: string
  rows: number
}

/** A grid cell travels as text; null stays null. */
export type DbCell = string | null

export interface DbRowsPage {
  columns: string[]
  /** The primary key columns — empty means the grid stays read-only. */
  key: string[]
  rows: DbCell[][]
  total: number
  limit: number
  offset: number
}

/** What a row edit sends: the path names the table, this names the row. */
export interface DbRowChange {
  key?: Record<string, string>
  values?: Record<string, string>
}

export interface DbQueryResult {
  columns: string[]
  rows: DbCell[][]
  affected: number
  truncated: boolean
}

// onUnauthorized lets the shell react when the session stops being accepted —
// without it, views would poll forever against a dead session.
let onUnauthorized: () => void = () => {}
export function setUnauthorizedHandler(handler: () => void) {
  onUnauthorized = handler
}

export class ApiError extends Error {
  status: number
  constructor(status: number, message: string) {
    super(message)
    this.status = status
  }
}

async function errorMessage(res: Response): Promise<string> {
  let message = `The server answered HTTP ${res.status}`
  try {
    const body = await res.json()
    if (body.error) message = body.error
  } catch {
    /* not JSON */
  }
  return message
}

// The donkey gateway in front of the console owns the session (an OIDC
// cookie); requests carry it automatically. A 401 means it expired — a full
// navigation lets the gateway run the sign-in again.
function unauthorized(): ApiError {
  onUnauthorized()
  return new ApiError(401, 'The session expired — signing in again.')
}

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const res = await fetch(path, {
    ...init,
    headers: {
      ...(init?.headers ?? {}),
      ...(init?.body ? { 'Content-Type': 'application/json' } : {}),
    },
  })
  if (res.status === 401) throw unauthorized()
  if (!res.ok) throw new ApiError(res.status, await errorMessage(res))
  return res.json() as Promise<T>
}

// Apps live in a space, so every app path carries it.
const appPath = (space: string, app: string, suffix = '') =>
  `/v1/spaces/${encodeURIComponent(space)}/apps/${encodeURIComponent(app)}${suffix}`

const servicePath = (space: string, service: string, suffix = '') =>
  `/v1/spaces/${encodeURIComponent(space)}/services/${encodeURIComponent(service)}${suffix}`

export const api = {
  // ping is the cheap "server up, token good" check — doctor runs the
  // expensive dependency probes and belongs to the System page alone.
  ping: () => request<{ ok: boolean }>('/v1/ping'),
  doctor: () => request<DoctorReport>('/v1/doctor'),
  apps: () => request<{ apps: AppSummary[] }>('/v1/apps'),
  status: async (space: string, app: string): Promise<AppStatus> => {
    const st = await request<AppStatus>(appPath(space, app, '/status'))
    return { ...st, pods: st.pods ?? [] }
  },
  builds: (space: string, app: string) =>
    request<{ builds: BuildSummary[] }>(appPath(space, app, '/builds')),
  build: (space: string, app: string, sequence: number) =>
    request<AppRelease>(appPath(space, app, `/builds/${sequence}`)),
  traffic: (
    space: string,
    app: string,
    source: TrafficSource = 'edge',
    range: TrafficRange = '30d',
  ) => {
    const query = `?source=${source}&range=${range}`
    return request<TrafficReport>(
      app
        ? `${appPath(space, app, '/traffic')}${query}`
        : `/v1/spaces/${encodeURIComponent(space)}/traffic${query}`,
    )
  },
  resources: (space: string, app: string, range: ResourceRange = '4h') =>
    request<ResourceReport>(`${appPath(space, app, '/resources')}?range=${range}`),
  env: (space: string, app: string) =>
    request<{ env: Record<string, string> }>(appPath(space, app, '/env')),
  setEnv: (space: string, app: string, env: Record<string, string>, restart: boolean) =>
    request<{ restarted: boolean; count: number }>(appPath(space, app, '/env'), {
      method: 'PUT',
      body: JSON.stringify({ env, restart }),
    }),
  destroy: (space: string, app: string, keepData: boolean) =>
    request<{ ok: boolean }>(`${appPath(space, app)}?keepData=${keepData}`, { method: 'DELETE' }),
  restart: (space: string, app: string) =>
    request<{ ok: boolean }>(appPath(space, app, '/restart'), { method: 'POST' }),
  network: (space: string, app: string) => request<NetworkInfo>(appPath(space, app, '/network')),
  // Changing exposure re-renders the app's routes on the spot — no deploy.
  // The choice sticks until donkey.yaml itself says otherwise.
  setNetwork: (space: string, app: string, options: NetworkOptions) =>
    request<{ network: NetworkInfo; note?: string }>(appPath(space, app, '/network'), {
      method: 'PUT',
      body: JSON.stringify(options),
    }),
  setAccess: (space: string, app: string, access: AccessConfig) =>
    request<{ ok: boolean; access: AccessConfig | null }>(appPath(space, app, '/access'), {
      method: 'PUT',
      body: JSON.stringify({
        users: access.users ?? [],
        groups: access.groups ?? [],
        roles: access.roles ?? [],
      }),
    }),
  // Every space, with or without apps — one that holds only services still
  // needs its card.
  platformConfig: () => request<PlatformConfig>('/v1/platform/settings'),
  setPlatformConfig: (settings: PlatformConfig) =>
    request<PlatformConfig>('/v1/platform/settings', {
      method: 'PUT', body: JSON.stringify(settings),
    }),
  spaces: () => request<SpaceList>('/v1/spaces'),
  createSpace: (name: string, settings: SpaceConfig) =>
    request<{ ok: boolean; name: string }>('/v1/spaces', {
      method: 'POST',
      body: JSON.stringify({ name, ...settings }),
    }),
  spaceConfig: (space: string) =>
    request<SpaceConfig>(`/v1/spaces/${encodeURIComponent(space)}/settings`),
  setSpaceConfig: (space: string, settings: SpaceConfig) =>
    request<SpaceConfig>(`/v1/spaces/${encodeURIComponent(space)}/settings`, {
      method: 'PUT', body: JSON.stringify(settings),
    }),
  // Removes the entire space boundary, including apps, services, and data.
  deleteSpace: (space: string) =>
    request<{ ok: boolean }>(`/v1/spaces/${encodeURIComponent(space)}`, { method: 'DELETE' }),
  addonTypes: () => request<{ types: AddonType[] }>('/v1/catalog'),
  // The binding with values — what the AI explorer chats with. Token-holder
  // territory, same trust level as the env API.
  serviceBinding: (space: string, service: string) =>
    request<{ binding: Record<string, string>; host: string; port: number }>(
      servicePath(space, service, '/binding'),
    ),
  // Services belong to the space; apps hold connections to them.
  spaceAddons: (space: string) =>
    request<{ addons: SpaceAddonEntry[] }>(`/v1/spaces/${encodeURIComponent(space)}/services`),
  // Creating can take a while the first time (the cluster pulls the image and
  // waits for the service to answer) — the request stays open throughout.
  addSpaceAddon: (space: string, req: AddAddonRequest) =>
    request<{ ok: boolean; version?: string }>(`/v1/spaces/${encodeURIComponent(space)}/services`, {
      method: 'POST',
      body: JSON.stringify(req),
    }),
  removeSpaceAddon: (space: string, addon: string, keepData: boolean) =>
    request<{ ok: boolean; detached?: string[] }>(
      `/v1/spaces/${encodeURIComponent(space)}/services/${encodeURIComponent(addon)}?keepData=${keepData}`,
      { method: 'DELETE' },
    ),
  addons: (space: string, app: string) =>
    request<{ addons: AddonEntry[] }>(appPath(space, app, '/services')),
  // Connects the app; with a type it creates the space service first when
  // missing (which can take a minute — the request stays open throughout).
  addAddon: (space: string, app: string, req: AddAddonRequest) =>
    request<{ ok: boolean; created?: boolean; restarted?: boolean; version?: string }>(
      appPath(space, app, '/services'),
      { method: 'POST', body: JSON.stringify(req) },
    ),
  // Disconnects the app; the service (and its data) stays with the space.
  removeAddon: (space: string, app: string, addon: string) =>
    request<{ ok: boolean }>(appPath(space, app, `/services/${encodeURIComponent(addon)}`), {
      method: 'DELETE',
    }),
  redisKeys: (space: string, service: string, pattern: string, cursor = '0') =>
    request<{ keys: RedisKey[]; cursor: string; db: number }>(servicePath(space, service, `/keys?${new URLSearchParams({ pattern, cursor })}`)),
  redisValue: (space: string, service: string, id: string) =>
    request<RedisValue>(servicePath(space, service, `/keys/value?${new URLSearchParams({ id })}`)),
  redisCreate: (space: string, service: string, name: string, value: string, ttl?: number) =>
    request<{ ok: boolean }>(servicePath(space, service, '/keys'), { method: 'POST', body: JSON.stringify({ name, value, ttl }) }),
  redisSave: (space: string, service: string, id: string, value: string, previous: string, ttl?: number) =>
    request<{ ok: boolean }>(servicePath(space, service, `/keys/value?${new URLSearchParams({ id })}`), { method: 'PUT', body: JSON.stringify({ value, previous, ttl }) }),
  redisDelete: (space: string, service: string, id: string) =>
    request<{ ok: boolean }>(servicePath(space, service, `/keys/value?${new URLSearchParams({ id })}`), { method: 'DELETE' }),
  // The database browser — talks to a space service's postgres through the
  // server; values travel as strings (or null), which is what a grid edits.
  // Containment lives in the path (service → database → table → rows);
  // postgres details like the schema ride the query, defaulting to public.
  dbDatabases: (space: string, service: string) =>
    request<{ databases: DbDatabase[]; default: string }>(
      servicePath(space, service, '/databases'),
    ),
  dbCreateDatabase: (space: string, service: string, name: string) =>
    request<{ ok: boolean; name: string }>(servicePath(space, service, '/databases'), {
      method: 'POST',
      body: JSON.stringify({ name }),
    }),
  dbDropDatabase: (space: string, service: string, db: string) =>
    request<{ ok: boolean }>(servicePath(space, service, `/databases/${encodeURIComponent(db)}`), {
      method: 'DELETE',
    }),
  dbTables: (space: string, service: string, db: string) =>
    request<{ tables: DbTable[] }>(
      servicePath(space, service, `/databases/${encodeURIComponent(db)}/tables`),
    ),
  dbDropTable: (space: string, service: string, db: string, schema: string, table: string) =>
    request<{ ok: boolean }>(
      servicePath(
        space,
        service,
        `/databases/${encodeURIComponent(db)}/tables/${encodeURIComponent(table)}?schema=${encodeURIComponent(schema)}`,
      ),
      { method: 'DELETE' },
    ),
  dbRows: (
    space: string,
    service: string,
    db: string,
    schema: string,
    table: string,
    limit: number,
    offset: number,
    order?: { column: string; desc: boolean },
  ) =>
    request<DbRowsPage>(
      servicePath(
        space,
        service,
        `/databases/${encodeURIComponent(db)}/tables/${encodeURIComponent(table)}/rows?schema=${encodeURIComponent(schema)}&limit=${limit}&offset=${offset}` +
          (order
            ? `&order=${encodeURIComponent(order.column)}&dir=${order.desc ? 'desc' : 'asc'}`
            : ''),
      ),
    ),
  dbInsertRow: (
    space: string,
    service: string,
    db: string,
    schema: string,
    table: string,
    body: DbRowChange,
  ) =>
    request<{ ok: boolean }>(
      servicePath(
        space,
        service,
        `/databases/${encodeURIComponent(db)}/tables/${encodeURIComponent(table)}/rows?schema=${encodeURIComponent(schema)}`,
      ),
      { method: 'POST', body: JSON.stringify(body) },
    ),
  dbUpdateRow: (
    space: string,
    service: string,
    db: string,
    schema: string,
    table: string,
    body: DbRowChange,
  ) =>
    request<{ ok: boolean; affected: number }>(
      servicePath(
        space,
        service,
        `/databases/${encodeURIComponent(db)}/tables/${encodeURIComponent(table)}/rows?schema=${encodeURIComponent(schema)}`,
      ),
      { method: 'PUT', body: JSON.stringify(body) },
    ),
  dbDeleteRow: (
    space: string,
    service: string,
    db: string,
    schema: string,
    table: string,
    body: DbRowChange,
  ) =>
    request<{ ok: boolean; affected: number }>(
      servicePath(
        space,
        service,
        `/databases/${encodeURIComponent(db)}/tables/${encodeURIComponent(table)}/rows?schema=${encodeURIComponent(schema)}`,
      ),
      { method: 'DELETE', body: JSON.stringify(body) },
    ),
  dbQuery: (space: string, service: string, db: string, sql: string) =>
    request<DbQueryResult>(
      servicePath(space, service, `/databases/${encodeURIComponent(db)}/query`),
      {
        method: 'POST',
        body: JSON.stringify({ sql }),
      },
    ),

  // diagnose asks the assistant to read the app's current evidence — this
  // can take a while (a model call with real logs behind it).
  diagnose: (space: string, app: string) =>
    request<Diagnosis>(appPath(space, app, '/diagnose'), { method: 'POST' }),

  // The app's disk. Paths are relative to it, with no leading slash.
  files: (space: string, app: string, path: string) =>
    request<FileListing>(appPath(space, app, `/files?path=${encodeURIComponent(path)}`)),
  // The download rides the session cookie like every other call; fetch keeps
  // the error handling uniform and hands back a blob.
  downloadFile: async (space: string, app: string, path: string): Promise<Blob> => {
    const res = await fetch(appPath(space, app, `/files/download?path=${encodeURIComponent(path)}`))
    if (res.status === 401) throw unauthorized()
    if (!res.ok) throw new ApiError(res.status, await errorMessage(res))
    return res.blob()
  },
  uploadFile: (space: string, app: string, path: string, file: File) => {
    const body = new FormData()
    body.append('file', file)
    return request<{ ok: boolean; path: string; size: number }>(
      appPath(space, app, `/files?path=${encodeURIComponent(path)}`),
      { method: 'POST', body },
    )
  },
  removeFile: (space: string, app: string, path: string) =>
    request<{ ok: boolean }>(appPath(space, app, `/files?path=${encodeURIComponent(path)}`), {
      method: 'DELETE',
    }),
  moveFile: (space: string, app: string, from: string, to: string) =>
    request<{ ok: boolean }>(
      appPath(
        space,
        app,
        `/files/move?from=${encodeURIComponent(from)}&to=${encodeURIComponent(to)}`,
      ),
      { method: 'POST' },
    ),
  makeFolder: (space: string, app: string, path: string) =>
    request<{ ok: boolean }>(
      appPath(space, app, `/files/folder?path=${encodeURIComponent(path)}`),
      {
        method: 'POST',
      },
    ),

  // plan uploads the project and asks the server to read it: how it would be
  // built, which port it listens on and why, what it needs, what will break.
  // Nothing is built and nothing on the cluster changes. It takes as long as
  // the assistant takes to read real code — tens of seconds is normal.
  plan: async (
    blob: Blob,
    opts: { space?: string; name?: string; path?: string },
  ): Promise<Plan> => {
    const params = new URLSearchParams()
    if (opts.space) params.set('space', opts.space)
    if (opts.name) params.set('name', opts.name)
    if (opts.path) params.set('path', opts.path)
    const res = await fetch(`/v1/plans?${params}`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/zip' },
      body: blob,
    })
    if (res.status === 401) throw unauthorized()
    if (!res.ok) throw new ApiError(res.status, await errorMessage(res))
    return res.json() as Promise<Plan>
  },
  // updatePlan settles the review screen's answers into the plan and re-runs
  // the checks against them — no upload, no second model call.
  updatePlan: (id: string, edit: PlanEdit) =>
    request<Plan>(`/v1/plans/${encodeURIComponent(id)}`, {
      method: 'PUT',
      body: JSON.stringify(edit),
    }),

  projectPlan: async (
    blob: Blob,
    opts: { space?: string; name?: string },
  ): Promise<ProjectPlan> => {
    const params = new URLSearchParams()
    if (opts.space) params.set('space', opts.space)
    if (opts.name) params.set('name', opts.name)
    const res = await fetch(`/v1/projects/plans?${params}`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/zip' },
      body: blob,
    })
    if (res.status === 401) throw unauthorized()
    if (!res.ok) throw new ApiError(res.status, await errorMessage(res))
    return res.json() as Promise<ProjectPlan>
  },
  updateProject: (id: string, edits: PlanEdit[]) =>
    request<ProjectPlan>(`/v1/projects/plans/${encodeURIComponent(id)}`, {
      method: 'PUT',
      body: JSON.stringify(edits),
    }),

  // deployUpload sends a zip build context and follows the NDJSON progress
  // stream — the same pipeline the CLI drives, fed from the browser. With a
  // plan id it carries the reading and the answers already reviewed, so the
  // assistant is not asked the same question twice.
  deployUpload: async (
    blob: Blob,
    opts: { space?: string; name?: string; port?: string; plan?: string; project?: boolean },
    onLine: (line: string) => void,
    onProgress: (progress: string) => void,
    onUpdate: (update: DeployUpdate) => void,
  ): Promise<DeployResult> => {
    const params = new URLSearchParams()
    if (opts.plan) params.set('plan', opts.plan)
    else {
      if (opts.space) params.set('space', opts.space)
      if (opts.name) params.set('name', opts.name)
      if (opts.port) params.set('port', opts.port)
    }
    const res = await fetch(`/v1/${opts.project ? 'projects/' : ''}deploys?${params}`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/zip' },
      body: blob,
    })
    if (res.status === 401) throw unauthorized()
    if (!res.ok) throw new ApiError(res.status, await errorMessage(res))
    if (!res.body) throw new ApiError(0, 'The server sent no deploy stream.')
    const reader = res.body.getReader()
    const decoder = new TextDecoder()
    let buffer = ''
    let result: DeployResult | null = null
    const handle = (line: string) => {
      if (!line.trim()) return
      try {
        const msg = JSON.parse(line)
        if (msg.result) result = msg.result
        else if (msg.line) onLine(msg.line)
        else if (msg.progress) onProgress(msg.progress)
        else if (msg.update) onUpdate(msg.update)
      } catch {
        /* partial line — wait for more */
      }
    }
    for (;;) {
      const { done, value } = await reader.read()
      if (done) break
      buffer += decoder.decode(value, { stream: true })
      const lines = buffer.split('\n')
      buffer = lines.pop() ?? ''
      lines.forEach(handle)
    }
    handle(buffer)
    if (!result) {
      throw new ApiError(
        0,
        'The connection broke mid-deploy — the server keeps working. Check the app page in a minute.',
      )
    }
    return result
  },

  // logs streams: onChunk receives text as the server flushes it. Returns a
  // cancel function. The server answers 200 before it knows whether pods
  // exist and reports late failures inside the body ("\nerror: …") — the
  // caller decides how to render those lines.
  logs: async (
    space: string,
    app: string,
    opts: { tail: number; follow?: boolean; since?: string },
    onChunk: (text: string) => void,
    signal?: AbortSignal,
  ): Promise<void> => {
    const params = new URLSearchParams({ tail: String(opts.tail) })
    if (opts.follow) params.set('follow', 'true')
    if (opts.since) params.set('since', opts.since)
    const res = await fetch(appPath(space, app, `/logs?${params}`), {
      signal,
    })
    if (res.status === 401) throw unauthorized()
    if (!res.ok) throw new ApiError(res.status, await errorMessage(res))
    if (!res.body) {
      onChunk(await res.text())
      return
    }
    const reader = res.body.getReader()
    const decoder = new TextDecoder()
    for (;;) {
      const { done, value } = await reader.read()
      if (done) break
      onChunk(decoder.decode(value, { stream: true }))
    }
  },
}

// Typed client for the donkey-server API. The session is the gateway's OIDC
// cookie and rides along on every request by itself; a 401 means it expired,
// and the registered handler reloads the page so the gateway signs in again.

/** How an app is reached: open to anyone, behind the login, or cluster-only. */
export type Exposure = 'public' | 'protected' | 'internal'
export type AppMode = 'web' | 'api'
export type CorsPolicy = '' | 'allowAll'

/** A deploy donkey-orchestrator is working on, or finished lately — an app
 * deployed for the first time exists nowhere else until it runs. */
export interface ActiveDeploy {
  space: string
  app: string
  stage: BoardStage
  /** The latest progress line of the deploy's log. */
  line?: string
  by?: string
  via: 'console' | 'cli'
  startedAt: string
  finishedAt?: string
  error?: string
}

export type BoardStage = 'queued' | DeployStage | 'done' | 'failed'

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
  /** What the app is written in, read off its build recipe ("python", "go"…). */
  stack?: string
  /** The vulnerability scan's outcome for the running image. */
  security?: AppSecurity
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
  /** The vulnerability scan's outcome for the running image. */
  security?: AppSecurity
  endpoint?: EndpointStatus
}

/** The scan's outcome as a running app carries it: one word, the numbers behind it. */
export interface AppSecurity {
  state: 'clean' | 'fixable' | 'warning' | 'waived' | 'needs-action'
  summary: ScanSummary
  scannedAt?: string
  dbUpdatedAt?: string
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
  authenticatedCallers: number
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
  endpoints: TrafficEndpoint[]
  mcp: {
    source: TrafficSource
    totals: OperationTotals
    operations: MCPOperation[]
  }
  definitions: {
    success: string
    view: string
    uniqueVisitors: string
    authenticatedCallers: string
  }
}

export interface TrafficEndpoint extends TrafficTotals {
  app: string
  method: string
  route: string
}

export interface OperationTotals {
  operations: number
  errors: number
  callerErrors: number
  averageLatencyMs: number
  p50LatencyMs: number
  p95LatencyMs: number
}

export interface MCPOperation extends OperationTotals {
  app: string
  method: string
  target: string
}

/**
 * Web analytics come from the script donkey-gateway adds to an app's HTML
 * pages: page views, visits and custom events, cookieless. A dimension is
 * both a breakdown and a filter.
 */
export type AnalyticsDimension =
  | 'path'
  | 'entry'
  | 'exit'
  | 'referrer'
  | 'channel'
  | 'utm_source'
  | 'utm_medium'
  | 'utm_campaign'
  | 'browser'
  | 'os'
  | 'device'
  | 'country'
  | 'language'
  | 'event'

export interface AnalyticsFilter {
  dimension: AnalyticsDimension
  value: string
}

export interface AnalyticsTotals {
  visitors: number
  visits: number
  pageviews: number
  events: number
  /** Pageviews per visit, one decimal. */
  viewsPerVisit: number
  /** Percent of visits with one page and nothing else; lower is better. */
  bounceRate: number
  visitDurationSeconds: number
}

export interface AnalyticsPoint {
  bucket: string
  visitors: number
  visits: number
  pageviews: number
}

/** `count` is pageviews for `path`, events for `event`, visits otherwise. */
export interface AnalyticsRow {
  value: string
  visitors: number
  count: number
}

export interface AnalyticsEventProperty {
  key: string
  values: { value: string; count: number }[]
}

export interface AnalyticsReport {
  space: string
  app: string
  range: TrafficRange
  granularity: 'hour' | 'day'
  from: string
  to: string
  filters: AnalyticsFilter[]
  totals: AnalyticsTotals
  /** The same-length window just before `from`; absent when not retained. */
  previous?: AnalyticsTotals
  series: AnalyticsPoint[]
  breakdowns: Partial<Record<AnalyticsDimension, AnalyticsRow[]>>
  /** Only with an `event` filter. */
  eventProperties?: AnalyticsEventProperty[]
  definitions: { visitor: string; visit: string; bounce: string; duration: string }
}

export interface AnalyticsLive {
  visitors: number
  windowMinutes: number
  pages: AnalyticsRow[]
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

/** Users and groups assigned a space or platform role. */
export interface AccessConfig {
  users?: string[]
  groups?: string[]
}

export interface SpaceConfig {
  admins: AccessConfig
  developers: AccessConfig
  viewers: AccessConfig
  allowPublic: boolean
  domains: string[]
  offerings?: Record<string, SpaceOffering>
  /** Space admins' decisions to deploy an app despite a scanner finding. */
  securityWaivers?: SecurityWaiver[]
}

export interface SecurityWaiver {
  app: string
  /** A CVE id, or a secret rule (optionally rule@file). */
  id: string
  reason: string
  by?: string
  /** A date (valid through that day) or an RFC 3339 time; empty never expires. */
  expires?: string
}

export interface SpaceOffering {
  defaults?: ServiceParameters
  config?: ServiceParameters
}

export interface ServiceHTTPRequest {
  method: string
  url: string
  headers: Record<string, string>
  body: string
}

export interface ServiceHTTPResponse {
  status: number
  statusText: string
  headers: Record<string, string[]>
  body: string
  truncated: boolean
  durationMs: number
  /** Bytes received, and the target actually requested. */
  size?: number
  url?: string
  /** A body that is not text arrives base64-encoded. */
  encoding?: 'base64'
}

export interface PlatformConfig {
  admins: AccessConfig
}

export interface SpaceList {
  spaces: string[]
  roles: Record<string, 'admin' | 'developer' | 'viewer'>
  allowPublic: Record<string, boolean>
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
  /** The gateway adds its analytics script to the app's HTML pages. On
   * unless the app opted out; only web apps with an address get it. */
  analytics: boolean
  url?: string
  /** The generated <app>.<space>.<domain> address — the input placeholder. */
  defaultHost: string
  /** Set when the app answers somewhere else: a name of its own under the
   * space, "@" for the space's own address, or a full hostname. */
  host?: string
  port?: number
  routes?: RouteEntry[]
  allowPublic: boolean
}

/** The parameter values a service was created with (version, disk size…). */
export type ServiceParameters = Record<string, unknown>

export interface AddonEntry {
  type?: string
  protocol?: string
  name: string
  keys: string[]
  parameters?: ServiceParameters
}

/** One service of a space, and the apps connected to it. */
export interface SpaceAddonEntry {
  type?: string
  protocol?: string
  name: string
  keys: string[]
  attachedTo: string[]
  parameters?: ServiceParameters
  /** Set only for a plugin instance without a binding: it is still being
   *  set up, failed (see message), or stopped with its data kept. */
  state?: 'pending' | 'failed' | 'stopped' | string
  message?: string
}

/** One property of a service's parameter schema — the JSON Schema subset
 * the catalog speaks, plus donkey's own presentation hints. */
export interface ParameterSchema {
  type?: 'string' | 'integer' | 'number' | 'boolean' | 'array'
  title?: string
  description?: string
  default?: unknown
  enum?: unknown[]
  minimum?: number
  maximum?: number
  pattern?: string
  /** "quantity" is a Kubernetes-style size such as 1Gi. */
  format?: string
  items?: ParameterSchema
  /** "disk-size" offers a ladder of sizes; "select" forces a dropdown. */
  'x-donkey-widget'?: string
  /** Cannot change once the service exists. */
  'x-donkey-immutable'?: boolean
  /** Hidden behind the advanced toggle. */
  'x-donkey-advanced'?: boolean
  /** Ascending sort key; unordered fields follow by name. */
  'x-donkey-order'?: number
}

export interface ParametersSchema {
  type?: string
  properties?: Record<string, ParameterSchema>
  required?: string[]
  additionalProperties?: boolean | object
}

export interface AddonEnvVar {
  name: string
  description?: string
  secret?: boolean
  optional?: boolean
}

/** One catalog entry: a service type donkey can provide — its descriptor,
 * the schema of what can be chosen, and the env contract connected apps
 * will see (names only, never values). */
export interface AddonType {
  /** Application protocol, independent of the service provisioner. */
  protocol?: string
  /** The lowercase id the API speaks. */
  type: string
  /** The configured provider an offering comes from; absent for built-ins. */
  provider?: string
  /** The product's own name for display (PostgreSQL, S3, OpenAI). */
  title: string
  description?: string
  /** A lowercase grouping: database, cache, storage, messaging, ai, api… */
  category?: string
  tags?: string[]
  /** An icon name overriding the protocol's icon. */
  icon?: string
  documentationUrl?: string
  /** Provisioning grants access to one installation-wide service rather
   *  than creating a dedicated instance. */
  shared?: boolean
  parameters?: ParametersSchema
  spaceConfiguration?: ParametersSchema
  common?: boolean
  env?: AddonEnvVar[]
  keys?: string[]
  capabilities?: { retainData: boolean; update: boolean }
  /** False while a provider has never answered; error says why. */
  available?: boolean
  error?: string
}

export interface AddAddonRequest {
  /** Omitted when connecting to a service the space already has. */
  type?: string
  name?: string
  /** Values of the type's parameter schema; missing ones take defaults. */
  parameters?: ServiceParameters
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
  analytics: boolean
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
  /** Where the recipe this build used sits in its commit. */
  recipe?: string
  changes?: SourceChange[]
  /** The vulnerability scan's outcome as soon as it is known (no inventory). */
  scan?: AppScan
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
  /** What the vulnerability scan found and what the policy made of it. */
  scan?: AppScan
  result: DeployResult
  truncated?: boolean
}

/** One known weakness in one package of the image. */
export interface Vulnerability {
  id: string
  severity: 'critical' | 'high' | 'medium' | 'low' | 'unknown'
  score?: number
  package: string
  installed: string
  fixed?: string
  status?: string
  class: string
  type: string
  target?: string
  path?: string
  title?: string
  description?: string
  url?: string
  published?: string
}

/** A credential-shaped value found in a file of the image (masked). */
export interface ImageSecret {
  target: string
  rule: string
  category?: string
  severity: string
  title: string
  line?: number
  match?: string
}

/** The assistant's reading of one finding. */
export interface Verdict {
  id: string
  verdict: 'affected' | 'not-affected' | 'unsure'
  confidence: 'high' | 'medium' | 'low'
  reason: string
  evidence: string[]
  fix?: string
}

export interface ScanSummary {
  critical: number
  high: number
  medium: number
  low: number
  unknown?: number
  fixable: number
  unfixed: number
  secrets: number
  blocking: number
  waived: number
  judged: number
}

export interface ScanPolicy {
  block: 'none' | 'critical' | 'high'
  ignoreUnfixed: boolean
  secrets: 'block' | 'warn' | 'off'
  triage: boolean
  waivers?: SecurityWaiver[]
}

export interface AppScan {
  scanner: string
  os?: string
  endOfLife?: boolean
  dbUpdatedAt?: string
  policy: ScanPolicy
  summary: ScanSummary
  findings: Finding[]
  vulnerabilities: Vulnerability[]
  secrets: ImageSecret[]
  verdicts?: Verdict[]
  blocked: boolean
  /** The scanner's CycloneDX inventory of the image, gzip + base64. */
  sbom?: string
}

/** One line of an app's build history, newest first. */
export interface BuildSummary {
  sequence: number
  created: string
  image?: string
  ok: boolean
}

// The source history: every deploy's upload as a commit by the person who
// deployed, and what donkey's build changed on top as a platform commit.
export type SourceKind = 'upload' | 'platform'

export interface SourceStats {
  files: number
  additions: number
  deletions: number
}

export interface SourceCommit {
  id: string
  parents: string[]
  kind: SourceKind
  author: { name: string; email?: string }
  date: string
  message: string
  /** The builds that used exactly this commit, newest first. `ok` is absent
   *  once the build's record aged out of the build history. */
  builds?: { sequence: number; ok?: boolean }[]
  stats: SourceStats
  /** What the app runs right now. */
  live?: boolean
}

/** One file a commit touched. (SourceChange is a build's repair diff.) */
export interface CommitChange {
  path: string
  status: 'added' | 'modified' | 'deleted'
  additions: number
  deletions: number
  binary?: boolean
  tooLarge?: boolean
}

export interface SourceCommitDetail extends SourceCommit {
  changes: CommitChange[]
}

export interface SourceFileDiff extends CommitChange {
  diff?: string
}

export interface SourceEntry {
  path: string
  size: number
  executable?: boolean
}

export interface SourceFile {
  text: string
  binary: boolean
  size: number
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

/** A database, or a Trino catalog; unavailable sizes are null. */
export interface DbDatabase {
  name: string
  size: number | null
}

/** A grid cell travels as text; null stays null. */
export type DbCell = string | null

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

// request, fetchOK and servicePath are exported for the service consoles,
// which keep their own calls beside them (see consoles.ts).
export async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const headers = new Headers(init?.headers)
  // FormData needs the browser's multipart content type and boundary.
  if (typeof init?.body === 'string' && !headers.has('Content-Type')) {
    headers.set('Content-Type', 'application/json')
  }
  const res = await fetch(path, {
    ...init,
    headers,
  })
  if (res.status === 401) throw unauthorized()
  if (!res.ok) throw new ApiError(res.status, await errorMessage(res))
  return res.json() as Promise<T>
}

// Downloads ride the session cookie like every other call; fetch keeps the
// error handling uniform and hands back a blob.
export async function fetchOK(path: string): Promise<Response> {
  const res = await fetch(path)
  if (res.status === 401) throw unauthorized()
  if (!res.ok) throw new ApiError(res.status, await errorMessage(res))
  return res
}

const downloadBlob = async (path: string): Promise<Blob> => (await fetchOK(path)).blob()

// saveBlob hands a fetched download to the browser under a name. Downloads go
// through fetch because they carry the session; a plain <a href> could not.
export function saveBlob(blob: Blob, name: string) {
  const url = URL.createObjectURL(blob)
  const a = document.createElement('a')
  a.href = url
  a.download = name
  a.click()
  URL.revokeObjectURL(url)
}

// Apps live in a space, so every app path carries it.
const appPath = (space: string, app: string, suffix = '') =>
  `/v1/spaces/${encodeURIComponent(space)}/apps/${encodeURIComponent(app)}${suffix}`

export const servicePath = (space: string, service: string, suffix = '') =>
  `/v1/spaces/${encodeURIComponent(space)}/services/${encodeURIComponent(service)}${suffix}`

export const api = {
  // ping is the cheap "server up, token good" check — doctor runs the
  // expensive dependency probes and belongs to the System page alone.
  ping: () => request<{ ok: boolean }>('/v1/ping'),
  doctor: () => request<DoctorReport>('/v1/doctor'),
  apps: () => request<{ apps: AppSummary[] }>('/v1/apps'),
  fleet: () => request<SpaceList & { apps: AppSummary[] }>('/v1/fleet'),
  deploys: async () => (await request<{ deploys: ActiveDeploy[] | null }>('/v1/deploys')).deploys ?? [],
  status: async (space: string, app: string): Promise<AppStatus> => {
    const st = await request<AppStatus>(appPath(space, app, '/status'))
    return { ...st, pods: st.pods ?? [] }
  },
  builds: (space: string, app: string) => request<{ builds: BuildSummary[] }>(appPath(space, app, '/builds')),
  build: (space: string, app: string, sequence: number) =>
    request<AppRelease>(appPath(space, app, `/builds/${sequence}`)),

  commits: (space: string, app: string) => request<{ commits: SourceCommit[] }>(appPath(space, app, '/commits')),
  commit: (space: string, app: string, id: string) =>
    request<SourceCommitDetail>(appPath(space, app, `/commits/${id}`)),
  commitDiff: (space: string, app: string, id: string, path: string) =>
    request<SourceFileDiff>(appPath(space, app, `/commits/${id}/diff?path=${encodeURIComponent(path)}`)),
  commitTree: (space: string, app: string, id: string) =>
    request<{ files: SourceEntry[] }>(appPath(space, app, `/commits/${id}/tree`)),
  // Files come back as plain text; the server flags binary ones so the
  // console offers a download instead of printing bytes.
  commitFile: async (space: string, app: string, id: string, path: string): Promise<SourceFile> => {
    const res = await fetchOK(appPath(space, app, `/commits/${id}/file?path=${encodeURIComponent(path)}`))
    const binary = res.headers.get('X-Donkey-Binary') === 'true'
    const blob = await res.blob()
    return { text: binary ? '' : await blob.text(), binary, size: blob.size }
  },
  downloadCommitFile: (space: string, app: string, id: string, path: string) =>
    downloadBlob(appPath(space, app, `/commits/${id}/file?path=${encodeURIComponent(path)}&download=true`)),
  downloadArchive: (space: string, app: string, id: string) =>
    downloadBlob(appPath(space, app, `/commits/${id}/archive`)),
  traffic: (space: string, app: string, source: TrafficSource = 'edge', range: TrafficRange = '30d') => {
    const query = `?source=${source}&range=${range}`
    return request<TrafficReport>(
      app ? `${appPath(space, app, '/traffic')}${query}` : `/v1/spaces/${encodeURIComponent(space)}/traffic${query}`,
    )
  },
  resources: (space: string, app: string, range: ResourceRange = '4h') =>
    request<ResourceReport>(`${appPath(space, app, '/resources')}?range=${range}`),
  // Filters repeat as filter=<dimension>:<value>; the server splits at the
  // first colon, so a value may carry colons of its own.
  analytics: async (
    space: string,
    app: string,
    range: TrafficRange = '7d',
    filters: AnalyticsFilter[] = [],
  ): Promise<AnalyticsReport> => {
    const query = new URLSearchParams({ range })
    for (const f of filters) query.append('filter', `${f.dimension}:${f.value}`)
    const report = await request<AnalyticsReport>(`${appPath(space, app, '/analytics')}?${query}`)
    return {
      ...report,
      filters: report.filters ?? [],
      series: report.series ?? [],
      breakdowns: report.breakdowns ?? {},
    }
  },
  analyticsLive: async (space: string, app: string): Promise<AnalyticsLive> => {
    const live = await request<AnalyticsLive>(appPath(space, app, '/analytics/live'))
    return { ...live, pages: live.pages ?? [] }
  },
  env: (space: string, app: string) => request<{ env: Record<string, string> }>(appPath(space, app, '/env')),
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
  platformConfig: () => request<PlatformConfig>('/v1/platform/settings'),
  setPlatformConfig: (settings: PlatformConfig) =>
    request<PlatformConfig>('/v1/platform/settings', {
      method: 'PUT',
      body: JSON.stringify(settings),
    }),
  spaces: () => request<SpaceList>('/v1/spaces'),
  createSpace: (name: string, settings: SpaceConfig) =>
    request<{ ok: boolean; name: string }>('/v1/spaces', {
      method: 'POST',
      body: JSON.stringify({ name, ...settings }),
    }),
  spaceConfig: (space: string) => request<SpaceConfig>(`/v1/spaces/${encodeURIComponent(space)}/settings`),
  setSpaceConfig: (space: string, settings: SpaceConfig) =>
    request<SpaceConfig>(`/v1/spaces/${encodeURIComponent(space)}/settings`, {
      method: 'PUT',
      body: JSON.stringify(settings),
    }),
  // Removes the entire space boundary, including apps, services, and data.
  deleteSpace: (space: string) =>
    request<{ ok: boolean }>(`/v1/spaces/${encodeURIComponent(space)}`, { method: 'DELETE' }),
  addonTypes: (space?: string) =>
    request<{ types: AddonType[] }>(space ? `/v1/spaces/${encodeURIComponent(space)}/catalog` : '/v1/catalog'),
  // The binding with values: same authorization as the app env API.
  serviceBinding: (space: string, service: string) =>
    request<{ binding: Record<string, string>; host: string; port: number; type?: string; protocol?: string }>(
      servicePath(space, service, '/binding'),
    ),
  // Services belong to the space; apps hold connections to them.
  spaceAddons: (space: string) =>
    request<{ addons: SpaceAddonEntry[] }>(`/v1/spaces/${encodeURIComponent(space)}/services`),
  // Creating can take a while the first time (the cluster pulls the image and
  // waits for the service to answer) — the request stays open throughout.
  addSpaceAddon: (space: string, req: AddAddonRequest) =>
    request<{ ok: boolean; parameters?: ServiceParameters }>(`/v1/spaces/${encodeURIComponent(space)}/services`, {
      method: 'POST',
      body: JSON.stringify(req),
    }),
  // orphan drops a plugin instance whose plugin is gone or failing; its
  // backing service is then the operator's to clean up.
  removeSpaceAddon: (space: string, addon: string, keepData: boolean, orphan = false) =>
    request<{ ok: boolean; detached?: string[] }>(
      `/v1/spaces/${encodeURIComponent(space)}/services/${encodeURIComponent(addon)}?keepData=${keepData}&orphan=${orphan}`,
      { method: 'DELETE' },
    ),
  addons: (space: string, app: string) => request<{ addons: AddonEntry[] }>(appPath(space, app, '/services')),
  // Connects the app; with a type it creates the space service first when
  // missing (which can take a minute — the request stays open throughout).
  addAddon: (space: string, app: string, req: AddAddonRequest) =>
    request<{ ok: boolean; created?: boolean; restarted?: boolean; parameters?: ServiceParameters }>(
      appPath(space, app, '/services'),
      { method: 'POST', body: JSON.stringify(req) },
    ),
  // Disconnects the app; the service (and its data) stays with the space.
  removeAddon: (space: string, app: string, addon: string) =>
    request<{ ok: boolean }>(appPath(space, app, `/services/${encodeURIComponent(addon)}`), {
      method: 'DELETE',
    }),
  // diagnose asks the assistant to read the app's current evidence — this
  // can take a while (a model call with real logs behind it).
  diagnose: (space: string, app: string) => request<Diagnosis>(appPath(space, app, '/diagnose'), { method: 'POST' }),

  // The app's disk. Paths are relative to it, with no leading slash.
  files: (space: string, app: string, path: string) =>
    request<FileListing>(appPath(space, app, `/files?path=${encodeURIComponent(path)}`)),
  // The download rides the session cookie like every other call; fetch keeps
  // the error handling uniform and hands back a blob.
  downloadFile: (space: string, app: string, path: string) =>
    downloadBlob(appPath(space, app, `/files/download?path=${encodeURIComponent(path)}`)),
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
      appPath(space, app, `/files/move?from=${encodeURIComponent(from)}&to=${encodeURIComponent(to)}`),
      { method: 'POST' },
    ),
  makeFolder: (space: string, app: string, path: string) =>
    request<{ ok: boolean }>(appPath(space, app, `/files/folder?path=${encodeURIComponent(path)}`), {
      method: 'POST',
    }),

  // plan uploads the project and asks the server to read it: how it would be
  // built, which port it listens on and why, what it needs, what will break.
  // Nothing is built and nothing on the cluster changes. It takes as long as
  // the assistant takes to read real code — tens of seconds is normal.
  plan: async (blob: Blob, opts: { space?: string; name?: string; path?: string }): Promise<Plan> => {
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

  projectPlan: async (blob: Blob, opts: { space?: string; name?: string }): Promise<ProjectPlan> => {
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

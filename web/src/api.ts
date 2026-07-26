// Typed client for the donkey-server API. The token lives in localStorage
// and every request carries it; a 401 clears it and flips the app back to
// the login screen via the registered handler.

/** How an app is reached: open to anyone, behind the login, or cluster-only. */
export type Exposure = 'public' | 'protected' | 'internal'

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
  port?: number
}

/** Who may open a login-protected app; empty lists mean "everyone signed in". */
export interface AccessConfig {
  users?: string[]
  groups?: string[]
  roles?: string[]
}

export interface RouteEntry {
  host: string
  path: string
}

export interface NetworkInfo {
  exposure: Exposure
  url?: string
  defaultHost: string
  domain?: string
  port?: number
  routes?: RouteEntry[]
  /** False when the installation has no identity provider — then there is no "protected". */
  loginAvailable: boolean
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
  type: string
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
  ok: boolean
  app: string
  space?: string
  imageRef?: string
  url?: string
  error?: string
  diagnosis?: string
  proposals?: FixAction[]
}

export interface DoctorCheck {
  name: string
  ok: boolean
  detail?: string
}

export interface DoctorReport {
  checks: DoctorCheck[]
  baseDomain: string
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

const TOKEN_KEY = 'donkey-token'

export function getToken(): string {
  return localStorage.getItem(TOKEN_KEY) ?? ''
}

export function setToken(token: string) {
  localStorage.setItem(TOKEN_KEY, token)
}

export function clearToken() {
  localStorage.removeItem(TOKEN_KEY)
}

// onUnauthorized lets the shell react when the token stops being accepted —
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

function unauthorized(): ApiError {
  clearToken()
  onUnauthorized()
  return new ApiError(401, 'The token was not accepted — sign in again.')
}

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const res = await fetch(path, {
    ...init,
    headers: {
      ...(init?.headers ?? {}),
      Authorization: `Bearer ${getToken()}`,
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
  setNetwork: (space: string, app: string, exposure: Exposure, domain: string) =>
    request<{ network: NetworkInfo; note?: string }>(appPath(space, app, '/network'), {
      method: 'PUT',
      body: JSON.stringify({ exposure, domain }),
    }),
  setAccess: (space: string, app: string, access: AccessConfig) =>
    request<{ ok: boolean; access: AccessConfig | null }>(appPath(space, app, '/access'), {
      method: 'PUT',
      body: JSON.stringify({ users: access.users ?? [], groups: access.groups ?? [], roles: access.roles ?? [] }),
    }),
  // Every space, with or without apps — one that holds only services still
  // needs its card.
  spaces: () => request<{ spaces: string[] }>('/v1/spaces'),
  createSpace: (name: string) =>
    request<{ ok: boolean; name: string }>('/v1/spaces', {
      method: 'POST',
      body: JSON.stringify({ name }),
    }),
  // Only for spaces without apps — their services and data go with it.
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
  // The database browser — talks to a space service's postgres through the
  // server; values travel as strings (or null), which is what a grid edits.
  // Containment lives in the path (service → database → table → rows);
  // postgres details like the schema ride the query, defaulting to public.
  dbDatabases: (space: string, service: string) =>
    request<{ databases: DbDatabase[]; default: string }>(servicePath(space, service, '/databases')),
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
          (order ? `&order=${encodeURIComponent(order.column)}&dir=${order.desc ? 'desc' : 'asc'}` : ''),
      ),
    ),
  dbInsertRow: (space: string, service: string, db: string, schema: string, table: string, body: DbRowChange) =>
    request<{ ok: boolean }>(
      servicePath(
        space,
        service,
        `/databases/${encodeURIComponent(db)}/tables/${encodeURIComponent(table)}/rows?schema=${encodeURIComponent(schema)}`,
      ),
      { method: 'POST', body: JSON.stringify(body) },
    ),
  dbUpdateRow: (space: string, service: string, db: string, schema: string, table: string, body: DbRowChange) =>
    request<{ ok: boolean; affected: number }>(
      servicePath(
        space,
        service,
        `/databases/${encodeURIComponent(db)}/tables/${encodeURIComponent(table)}/rows?schema=${encodeURIComponent(schema)}`,
      ),
      { method: 'PUT', body: JSON.stringify(body) },
    ),
  dbDeleteRow: (space: string, service: string, db: string, schema: string, table: string, body: DbRowChange) =>
    request<{ ok: boolean; affected: number }>(
      servicePath(
        space,
        service,
        `/databases/${encodeURIComponent(db)}/tables/${encodeURIComponent(table)}/rows?schema=${encodeURIComponent(schema)}`,
      ),
      { method: 'DELETE', body: JSON.stringify(body) },
    ),
  dbQuery: (space: string, service: string, db: string, sql: string) =>
    request<DbQueryResult>(servicePath(space, service, `/databases/${encodeURIComponent(db)}/query`), {
      method: 'POST',
      body: JSON.stringify({ sql }),
    }),

  // diagnose asks the assistant to read the app's current evidence — this
  // can take a while (a model call with real logs behind it).
  diagnose: (space: string, app: string) =>
    request<Diagnosis>(appPath(space, app, '/diagnose'), { method: 'POST' }),

  // The app's disk. Paths are relative to it, with no leading slash.
  files: (space: string, app: string, path: string) =>
    request<FileListing>(appPath(space, app, `/files?path=${encodeURIComponent(path)}`)),
  // The download carries the token like every other call, so it goes through
  // fetch and comes back as a blob — a plain <a href> could not authenticate.
  downloadFile: async (space: string, app: string, path: string): Promise<Blob> => {
    const res = await fetch(appPath(space, app, `/files/download?path=${encodeURIComponent(path)}`), {
      headers: { Authorization: `Bearer ${getToken()}` },
    })
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
      appPath(space, app, `/files/move?from=${encodeURIComponent(from)}&to=${encodeURIComponent(to)}`),
      { method: 'POST' },
    ),
  makeFolder: (space: string, app: string, path: string) =>
    request<{ ok: boolean }>(appPath(space, app, `/files/folder?path=${encodeURIComponent(path)}`), {
      method: 'POST',
    }),

  // deployUpload sends a zip build context and follows the NDJSON progress
  // stream — the same pipeline the CLI drives, fed from the browser.
  deployUpload: async (
    blob: Blob,
    opts: { space?: string; name?: string; port?: string },
    onLine: (line: string) => void,
    onProgress: (progress: string) => void,
  ): Promise<DeployResult> => {
    const params = new URLSearchParams()
    if (opts.space) params.set('space', opts.space)
    if (opts.name) params.set('name', opts.name)
    if (opts.port) params.set('port', opts.port)
    const res = await fetch(`/v1/deploys?${params}`, {
      method: 'POST',
      headers: { Authorization: `Bearer ${getToken()}`, 'Content-Type': 'application/zip' },
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
      headers: { Authorization: `Bearer ${getToken()}` },
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

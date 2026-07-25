// Typed client for the donkey-server API. The token lives in localStorage
// and every request carries it; a 401 clears it so the login screen returns.

export interface AppSummary {
  name: string
  healthy: boolean
  ready: string
  image: string
  url: string
}

export interface AppStatus {
  found: boolean
  healthy: boolean
  ready: string
  image: string
  url: string
  pods: string[]
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

export class ApiError extends Error {
  status: number
  constructor(status: number, message: string) {
    super(message)
    this.status = status
  }
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
  if (res.status === 401) {
    clearToken()
    throw new ApiError(401, 'The token was not accepted — sign in again.')
  }
  if (!res.ok) {
    let message = `The server answered HTTP ${res.status}`
    try {
      const body = await res.json()
      if (body.error) message = body.error
    } catch {
      /* not JSON */
    }
    throw new ApiError(res.status, message)
  }
  return res.json() as Promise<T>
}

export const api = {
  doctor: () => request<DoctorReport>('/v1/doctor'),
  apps: () => request<{ apps: AppSummary[] }>('/v1/apps'),
  status: (app: string) => request<AppStatus>(`/v1/apps/${app}/status`),
  env: (app: string) => request<{ env: Record<string, string> }>(`/v1/apps/${app}/env`),
  setEnv: (app: string, env: Record<string, string>, restart: boolean) =>
    request<{ restarted: boolean }>(`/v1/apps/${app}/env`, {
      method: 'PUT',
      body: JSON.stringify({ env, restart }),
    }),
  destroy: (app: string, keepData: boolean) =>
    request<{ ok: boolean }>(`/v1/apps/${app}?keepData=${keepData}`, { method: 'DELETE' }),
  logs: async (app: string, tail: number): Promise<string> => {
    const res = await fetch(`/v1/apps/${app}/logs?tail=${tail}`, {
      headers: { Authorization: `Bearer ${getToken()}` },
    })
    if (res.status === 401) {
      clearToken()
      throw new ApiError(401, 'The token was not accepted — sign in again.')
    }
    if (!res.ok) throw new ApiError(res.status, `The server answered HTTP ${res.status}`)
    return res.text()
  },
}

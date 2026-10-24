import { useSyncExternalStore } from 'react'

// The API consoles (HTTP, AI, Exa) keep a few per-viewer conveniences — saved
// requests, history, conversations, recent searches — in this browser's
// localStorage. Every access may throw (private mode, a full quota, storage
// blocked by policy) and a stored value may be anything an older console
// wrote, so reads validate and writes give up quietly: losing a history is
// fine, breaking the console over it is not.
//
// A value lives in a box shared by everything that shows it, outside React:
// a request finishing or an answer streaming in after its tab was left still
// lands where the next render finds it.

export type Box<T> = {
  get: () => T
  set: (next: T | ((prev: T) => T)) => void
  subscribe: (listener: () => void) => () => void
}

type BoxOptions<T> = {
  /** Kept in memory only — for unsaved edits and live state. */
  memory?: boolean
  /** The most a stored value may take, as JSON. */
  maxBytes?: number
  /** Makes a value smaller (drops its oldest entry); undefined once it cannot. */
  shrink?: (value: T) => T | undefined
}

const boxes = new Map<string, Box<unknown>>()

// Writes are coalesced: a keystroke in a request body or a system prompt
// changes the value at once, and storage a moment later — and before the
// page goes away.
const pending = new Map<string, () => void>()
const flushAll = () => {
  for (const write of [...pending.values()]) write()
}
if (typeof window !== 'undefined') window.addEventListener('pagehide', flushAll)

/**
 * The box for a key, made on first use. `parse` turns whatever is stored
 * into a valid value (or the default).
 */
export function box<T>(key: string, parse: (raw: unknown) => T, options: BoxOptions<T> = {}): Box<T> {
  const known = boxes.get(key)
  if (known) return known as Box<T>
  const listeners = new Set<() => void>()
  const notify = () => {
    for (const listener of listeners) listener()
  }
  let value = parse(options.memory ? undefined : load(key))
  let timer = 0
  const write = () => {
    clearTimeout(timer)
    pending.delete(key)
    const kept = save(key, value, options)
    // What did not fit is gone from memory too, so both agree.
    if (kept !== value) {
      value = kept
      notify()
    }
  }
  const made: Box<T> = {
    get: () => value,
    set: (next) => {
      value = typeof next === 'function' ? (next as (prev: T) => T)(value) : next
      notify()
      if (options.memory) return
      clearTimeout(timer)
      timer = setTimeout(write, 300)
      pending.set(key, write)
    },
    subscribe: (listener) => {
      listeners.add(listener)
      return () => listeners.delete(listener)
    },
  }
  boxes.set(key, made as Box<unknown>)
  return made
}

export function useBox<T>(b: Box<T>): T {
  return useSyncExternalStore(b.subscribe, b.get, b.get)
}

function load(key: string): unknown {
  try {
    const raw = localStorage.getItem(key)
    return raw === null ? undefined : JSON.parse(raw)
  } catch {
    return undefined
  }
}

// save stores a value within its budget and returns what was kept — the
// value shrunk until it fits, so memory and storage agree.
function save<T>(key: string, value: T, { maxBytes = 256 * 1024, shrink }: BoxOptions<T>): T {
  let kept: T | undefined = value
  while (kept !== undefined) {
    const text = JSON.stringify(kept)
    if (text.length <= maxBytes) {
      try {
        localStorage.setItem(key, text)
      } catch {
        /* storage unavailable or full — the value still holds for this tab */
      }
      return kept
    }
    kept = shrink?.(kept)
  }
  try {
    localStorage.removeItem(key)
  } catch {
    /* as above */
  }
  return value
}

/** Drops a list's last (oldest) entry; lists are kept newest first. */
export const dropOldest = <T>(list: T[]): T[] | undefined => (list.length > 0 ? list.slice(0, -1) : undefined)

/** An id for a stored entry. */
export function newId(): string {
  try {
    return crypto.randomUUID().slice(0, 8)
  } catch {
    return Math.random().toString(36).slice(2, 10)
  }
}

// The parse helpers: a stored list keeps only the entries that still have
// the shape the console expects.
export const isRecord = (v: unknown): v is Record<string, unknown> => typeof v === 'object' && v !== null && !Array.isArray(v)
export const str = (v: unknown, fallback = ''): string => (typeof v === 'string' ? v : fallback)
export const num = (v: unknown): number | undefined => (typeof v === 'number' && Number.isFinite(v) ? v : undefined)
export function listOf<T>(raw: unknown, item: (v: Record<string, unknown>) => T | undefined): T[] {
  if (!Array.isArray(raw)) return []
  const out: T[] = []
  for (const v of raw) {
    const parsed = isRecord(v) ? item(v) : undefined
    if (parsed !== undefined) out.push(parsed)
  }
  return out
}

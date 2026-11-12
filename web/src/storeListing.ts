import type { StoreListing, StoreObject } from './storeApi'

export type StoreRow = { id: string; key: string; label: string; folder: boolean; object?: StoreObject }
export type StoreSort = { by: 'name' | 'size' | 'modified'; desc: boolean }

/** Keys are already decoded by the store; decoding again would alter literal percent escapes. */
export const lastSegment = (key: string) => key.replace(/\/$/, '').split('/').pop() ?? key

/** Labels belong to keys, never the other way around: display names need not be unique. */
export function listingNames(pages: StoreListing[]): Map<string, string> {
  return new Map(pages.flatMap((page) => Object.entries(page.displayNames ?? {})))
}

export const storeLabel = (key: string, names: Map<string, string>) => names.get(key) || lastSegment(key)

export function storeRows(pages: StoreListing[], filter: string, sort: StoreSort): StoreRow[] {
  const names = listingNames(pages)
  const folders = [...new Set(pages.flatMap((p) => p.prefixes))]
  const objects = [...new Map(pages.flatMap((p) => p.objects).map((o) => [o.key, o])).values()]
  const needle = filter.trim().toLowerCase()
  const keep = (key: string) => storeLabel(key, names).toLowerCase().includes(needle)
  const dir = sort.desc ? -1 : 1
  const byName = (a: string, b: string) =>
    storeLabel(a, names).localeCompare(storeLabel(b, names), undefined, { numeric: true }) || a.localeCompare(b)

  return [
    ...folders
      .filter(keep)
      .sort((a, b) => (sort.by === 'name' ? dir : 1) * byName(a, b))
      .map((key) => ({ id: `p:${key}`, key, label: storeLabel(key, names), folder: true })),
    ...objects
      .filter((o) => keep(o.key))
      .sort((a, b) => {
        const d =
          sort.by === 'size'
            ? a.size - b.size
            : sort.by === 'modified'
              ? a.modified.localeCompare(b.modified)
              : byName(a.key, b.key)
        return dir * (d || byName(a.key, b.key))
      })
      .map((o) => ({ id: `o:${o.key}`, key: o.key, label: storeLabel(o.key, names), folder: false, object: o })),
  ]
}

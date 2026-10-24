import { useNavigate, useParams, useSearch } from '@tanstack/react-router'

// A service console's place in the URL — which table, folder, key or saved
// request is open — so back and forward walk it and every view can be
// linked. Each console names its own keys; values are strings.
export type ServiceSearch = Record<string, string>

export function cleanServiceSearch(search: Record<string, unknown>): ServiceSearch {
  const out: ServiceSearch = {}
  for (const [key, value] of Object.entries(search)) {
    if (typeof value === 'string' && value !== '') out[key] = value
    else if (typeof value === 'number') out[key] = String(value)
  }
  return out
}

/**
 * The console's place, and a way to move it. Moving makes a history step, so
 * back returns to the previous table or folder; `replace` is for refinements
 * that are not a place of their own (a filter as it is typed).
 */
export function useServiceSearch(): [ServiceSearch, (next: ServiceSearch, how?: 'push' | 'replace') => void] {
  const search = useSearch({ strict: false }) as ServiceSearch
  const { space = '', name = '', tab } = useParams({ strict: false }) as { space?: string; name?: string; tab?: string }
  const navigate = useNavigate()
  const move = (next: ServiceSearch, how: 'push' | 'replace' = 'push') =>
    navigate({
      to: '/service/$space/$name/{-$tab}',
      params: { space, name, tab },
      search: cleanServiceSearch(next),
      replace: how === 'replace',
      resetScroll: false,
    })
  return [search, move]
}

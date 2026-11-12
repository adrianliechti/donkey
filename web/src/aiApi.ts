import { queryOptions, useQuery } from '@tanstack/react-query'
import { request, servicePath } from './api'
import { isRecord } from './httpStore'
import { useServiceBinding } from './queries'

// The models of an OpenAI-compatible service, for the console's quick chat.
// Calls go to the service's console routes (…/ai/models), where the server
// swaps the session for the service's own key.

/** A model as the service lists it; providers add fields of their own. */
export type Model = { id: string; owned_by?: string; created?: number } & Record<string, unknown>

export const modelsQuery = (space: string, service: string) =>
  queryOptions({
    queryKey: ['ai', space, service, 'models'],
    queryFn: async ({ signal }) => {
      const answer = await request<{ data?: unknown; models?: unknown }>(servicePath(space, service, '/ai/models'), {
        signal,
      })
      const list = Array.isArray(answer.data) ? answer.data : Array.isArray(answer.models) ? answer.models : []
      return list
        .filter((m): m is Model => isRecord(m) && typeof m.id === 'string' && m.id !== '')
        .sort((a, b) => a.id.localeCompare(b.id))
    },
    staleTime: 5 * 60_000,
    // A service without a model listing is common and not worth asking twice.
    retry: false,
  })

export type ModelChoices = { ids: string[]; bound: string; preferred: string; loading: boolean }

/** The models a conversation can use: the binding's own first. */
export function useModelChoices(space: string, service: string): ModelChoices {
  const { data: binding } = useServiceBinding(space, service)
  const listed = useQuery(modelsQuery(space, service))
  // The binding's model leads: it is the one the platform vouches for,
  // whether or not the upstream's own listing mentions it.
  const bound = binding?.binding.OPENAI_MODEL ?? ''
  const ids = [...new Set([...(bound ? [bound] : []), ...(listed.data ?? []).map((m) => m.id)])]
  return { ids, bound, preferred: bound || ids[0] || '', loading: listed.isPending }
}

// The contacts console's calls and their caches. A CardDAV service holds
// address books, each a set of vCards; the server reads them as the signed-in
// user and hands back what a card says, already parsed. DAV keys travel in
// the query unchanged, including paths for discovered sibling collections.
import { queryOptions, useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { request, servicePath } from './api'

export interface AddressBook {
  key: string
  name: string
  description?: string
}

/** One value of a card — an address, a number — and what it is for. */
export type Typed = { value: string; types?: string[]; preferred?: boolean }

export type ContactKind = 'individual' | 'group' | 'org' | 'location'

export interface ContactSummary {
  key: string
  etag?: string
  name: string
  org?: string
  email?: string
  phone?: string
  kind?: ContactKind
}

/** A card read in full. Only the detail brings the photo: the list asks for none. */
export type ContactDetail = ContactSummary & {
  names?: { prefix?: string; given?: string; middle?: string; family?: string; suffix?: string }
  nickname?: string
  title?: string
  role?: string
  department?: string
  emails: Typed[]
  phones: Typed[]
  /** Each address's lines, joined by "\n". */
  addresses: Typed[]
  urls: Typed[]
  /** "YYYY-MM-DD", "--MM-DD" without a year, or as written. */
  birthday?: string
  anniversary?: string
  note?: string
  categories: string[]
  /** A data: URI — only photos the card holds itself. */
  photo?: string
  members?: string[]
  uid?: string
  revision?: string
  version?: string
  /** The vCard as stored. */
  data: string
}

const path = (space: string, service: string, suffix: string, query: Record<string, string>) =>
  servicePath(space, service, `${suffix}?${new URLSearchParams(query)}`)

export const contactsApi = {
  books: async (space: string, service: string, signal?: AbortSignal) =>
    (await request<{ addressBooks: AddressBook[] | null }>(servicePath(space, service, '/addressbooks'), { signal }))
      .addressBooks ?? [],
  contacts: async (space: string, service: string, book: string, signal?: AbortSignal) => {
    const page = await request<{ contacts: ContactSummary[] | null; truncated: boolean }>(
      path(space, service, '/contacts', { book }),
      { signal },
    )
    return { contacts: page.contacts ?? [], truncated: page.truncated }
  },
  contact: async (space: string, service: string, key: string, signal?: AbortSignal) => {
    const c = await request<ContactDetail>(path(space, service, '/contacts/item', { key }), { signal })
    return {
      ...c,
      emails: c.emails ?? [],
      phones: c.phones ?? [],
      addresses: c.addresses ?? [],
      urls: c.urls ?? [],
      categories: c.categories ?? [],
    }
  },
  remove: (space: string, service: string, key: string, etag = '') =>
    request<{ ok: boolean }>(path(space, service, '/contacts/item', etag ? { key, etag } : { key }), {
      method: 'DELETE',
    }),
}

// ─── Queries ───────────────────────────────────────────────────────────

const root = (space: string, service: string) => ['contacts', space, service] as const

export const addressBooksQuery = (space: string, service: string) =>
  queryOptions({
    queryKey: [...root(space, service), 'books'],
    queryFn: ({ signal }) => contactsApi.books(space, service, signal),
  })

export const contactListQuery = (space: string, service: string, book: string) =>
  queryOptions({
    queryKey: [...root(space, service), 'list', book],
    queryFn: ({ signal }) => contactsApi.contacts(space, service, book, signal),
  })

export const contactQuery = (space: string, service: string, key: string) =>
  queryOptions({
    queryKey: [...root(space, service), 'card', key],
    queryFn: ({ signal }) => contactsApi.contact(space, service, key, signal),
    retry: (count, err) => (err as { status?: number }).status !== 404 && count < 2,
  })

export function useAddressBooks(space: string, service: string) {
  return useQuery(addressBooksQuery(space, service))
}

export function useContactList(space: string, service: string, book: string | undefined) {
  return useQuery({ ...contactListQuery(space, service, book ?? ''), enabled: book !== undefined })
}

export function useContact(space: string, service: string, key: string) {
  return useQuery(contactQuery(space, service, key))
}

/**
 * Deleting a card refreshes the book it was in. The card's own cache stays
 * while its pane is still open — dropping it there would read the deleted
 * card again — and a later visit finds it gone (404).
 */
export function useDeleteContact(space: string, service: string) {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: ({ key, etag }: { key: string; etag?: string }) => contactsApi.remove(space, service, key, etag),
    onSuccess: () => qc.invalidateQueries({ queryKey: [...root(space, service), 'list'] }),
  })
}

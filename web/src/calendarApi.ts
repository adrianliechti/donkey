// The calendar console's calls and their caches. A CalDAV service holds
// calendars of events; the server asks it for a window of time as the
// signed-in user, expands what repeats into its occurrences, and hands back
// each one parsed. The query carries DAV keys unchanged. Floating times
// are read in the viewer's own zone, which every call names.
import { queryOptions, useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { request, servicePath } from './api'

export interface Calendar {
  key: string
  name: string
  description?: string
  /** "#rrggbb", as the calendar's owner picked it. */
  color?: string
}

export type EventStatus = 'tentative' | 'confirmed' | 'cancelled'

export interface EventSummary {
  key: string
  etag?: string
  uid?: string
  /** This occurrence's RECURRENCE-ID; absent for a one-off event. */
  instance?: string
  summary: string
  /** Timed: RFC 3339 in UTC. All-day: "YYYY-MM-DD", the end exclusive. */
  start: string
  end: string
  allDay: boolean
  location?: string
  status?: EventStatus
  /** One occurrence of a series. */
  recurring: boolean
  /** Does not block time (TRANSP:TRANSPARENT). */
  free?: boolean
}

export type Person = { name?: string; email?: string }

export type AttendeeStatus = 'needs-action' | 'accepted' | 'declined' | 'tentative' | 'delegated'

export interface EventDetail extends EventSummary {
  description?: string
  url?: string
  organizer?: Person
  attendees: (Person & { status?: AttendeeStatus; role?: string })[]
  categories: string[]
  /** The series' RRULE as written. */
  rule?: string
  created?: string
  modified?: string
  class?: string
  /** The .ics as stored. */
  data: string
}

/** One occurrence, wherever it shows: a series shares its key. */
export const occurrenceId = (e: { key: string; instance?: string }) => `${e.key}\n${e.instance ?? ''}`

/** The viewer's zone, for the times an event leaves floating. */
export const viewerZone = () => Intl.DateTimeFormat().resolvedOptions().timeZone || 'UTC'

const path = (space: string, service: string, suffix: string, query: Record<string, string>) =>
  servicePath(space, service, `${suffix}?${new URLSearchParams(query)}`)

export const calendarApi = {
  calendars: async (space: string, service: string, signal?: AbortSignal) =>
    (await request<{ calendars: Calendar[] | null }>(servicePath(space, service, '/calendars'), { signal }))
      .calendars ?? [],
  events: async (
    space: string,
    service: string,
    calendar: string,
    start: Date,
    end: Date,
    tz: string,
    signal?: AbortSignal,
  ) => {
    const page = await request<{ events: EventSummary[] | null; truncated: boolean }>(
      path(space, service, '/events', { calendar, start: start.toISOString(), end: end.toISOString(), tz }),
      { signal },
    )
    return { events: page.events ?? [], truncated: page.truncated }
  },
  event: async (space: string, service: string, key: string, instance: string, tz: string, signal?: AbortSignal) => {
    const e = await request<EventDetail>(
      path(space, service, '/events/item', instance ? { key, instance, tz } : { key, tz }),
      { signal },
    )
    return { ...e, attendees: e.attendees ?? [], categories: e.categories ?? [] }
  },
  remove: (space: string, service: string, key: string, etag = '') =>
    request<{ ok: boolean }>(path(space, service, '/events/item', etag ? { key, etag } : { key }), {
      method: 'DELETE',
    }),
}

// ─── Queries ───────────────────────────────────────────────────────────

const root = (space: string, service: string) => ['calendar', space, service] as const

export const calendarsQuery = (space: string, service: string) =>
  queryOptions({
    queryKey: [...root(space, service), 'calendars'],
    queryFn: ({ signal }) => calendarApi.calendars(space, service, signal),
  })

export const eventsQuery = (space: string, service: string, calendar: string, start: Date, end: Date, tz: string) =>
  queryOptions({
    queryKey: [...root(space, service), 'events', calendar, start.toISOString(), end.toISOString(), tz],
    queryFn: ({ signal }) => calendarApi.events(space, service, calendar, start, end, tz, signal),
  })

export const eventQuery = (space: string, service: string, key: string, instance: string, tz: string) =>
  queryOptions({
    queryKey: [...root(space, service), 'event', key, instance, tz],
    queryFn: ({ signal }) => calendarApi.event(space, service, key, instance, tz, signal),
    retry: (count, err) => (err as { status?: number }).status !== 404 && count < 2,
  })

export function useCalendars(space: string, service: string) {
  return useQuery(calendarsQuery(space, service))
}

export function useEvents(
  space: string,
  service: string,
  calendar: string | undefined,
  start: Date,
  end: Date,
  tz: string,
) {
  return useQuery({ ...eventsQuery(space, service, calendar ?? '', start, end, tz), enabled: calendar !== undefined })
}

export function useEvent(space: string, service: string, key: string, instance: string, tz: string) {
  return useQuery(eventQuery(space, service, key, instance, tz))
}

/**
 * Deleting an event removes the whole resource: every occurrence of a
 * series. The windows are read again; the event's own cache stays while its
 * pane is still open, and a later visit finds it gone (404).
 */
export function useDeleteEvent(space: string, service: string) {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: ({ key, etag }: { key: string; etag?: string }) => calendarApi.remove(space, service, key, etag),
    onSuccess: () => qc.invalidateQueries({ queryKey: [...root(space, service), 'events'] }),
  })
}

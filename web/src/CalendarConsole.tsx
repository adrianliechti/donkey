import { useEffect, useMemo, useState, type ReactNode } from 'react'
import { useQueryClient } from '@tanstack/react-query'
import {
  ArrowLeft,
  CalendarDays,
  ChevronLeft,
  ChevronRight,
  Download,
  RefreshCw,
  Repeat,
  Trash2,
  X,
} from 'lucide-react'
import type { ConsoleProps } from './consoles'
import {
  eventsQuery,
  occurrenceId,
  useCalendars,
  useDeleteEvent,
  useEvent,
  useEvents,
  viewerZone,
  type AttendeeStatus,
  type Calendar,
  type EventDetail,
  type EventSummary,
  type Person,
} from './calendarApi'
import {
  agendaDays,
  dayKey,
  dayTitle,
  describeRule,
  firstDayOfWeek,
  parseDay,
  rowTime,
  stepWindow,
  viewWindow,
  whenText,
  windowTitle,
  type CalendarView,
} from './calendarDates'
import { Modal, ModalBody, ModalFooter, ModalHeader } from './dialog'
import { saveBlob } from './api'
import { downloadName } from './fileName'
import { useNow } from './hooks'
import { mutationError } from './queries'
import { useServiceSearch, type ServiceSearch } from './serviceSearch'
import { PaneEmpty, PaneHeader, Row, Segmented } from './Workbench'
import { Button, Chip, ErrorNote, Eyebrow, IconButton, Spinner, Toolbar } from './ui'
import { errText } from './format'

// The calendar console: the calendars on the left, a week or a month of the
// picked one as an agenda in the middle — the days with something on, and
// today — and the event picked on the right. What repeats arrives expanded:
// each occurrence is a row of its own. Everything is read as the signed-in
// user. Where it stands — calendar, window, event — lives in the URL.
type Place = { calendar?: string; view?: string; from?: string; event?: string; instance?: string }

export function Console({ space, service }: ConsoleProps) {
  const [search, move] = useServiceSearch()
  const place = search as Place
  const go = (next: Place, how?: 'push' | 'replace') => move(next as ServiceSearch, how)
  const calendars = useCalendars(space, service)
  // The first calendar is where the console opens; its key may be the root ("").
  const wanted = place.calendar ?? calendars.data?.[0]?.key
  const calendar = calendars.data?.find((c) => c.key === wanted)
  const view: CalendarView = place.view === 'month' ? 'month' : 'week'
  const firstDay = useMemo(() => firstDayOfWeek(), [])
  const tz = useMemo(() => viewerZone(), [])
  const now = new Date(useNow(60_000))
  // The window holds `from`, or today; it starts where the view starts.
  const anchor = place.from && parseDay(place.from) ? place.from : dayKey(now)
  const { start, end } = useMemo(() => viewWindow(view, parseDay(anchor)!, firstDay), [view, anchor, firstDay])
  const here: Place = { calendar: place.calendar, view: place.view, from: place.from }

  // A service URL that is the calendar itself has nothing to pick from: the
  // console opens on its agenda, as the file browser opens a WebDAV tree.
  const fixed = calendars.data?.length === 1 && calendars.data[0].key === ''
  // A narrow screen shows one pane at a time: the calendars until one is
  // picked, then its agenda, then the event.
  const picked = fixed || place.calendar !== undefined || place.event !== undefined
  const previewing = place.event !== undefined
  const opened = previewing ? occurrenceId({ key: place.event!, instance: place.instance }) : undefined

  return (
    <Row>
      {!fixed && (
        <CalendarList
          calendars={calendars}
          current={calendar?.key}
          onPick={(key) => go({ calendar: key || undefined, view: place.view, from: place.from })}
          className={picked ? 'hidden md:flex' : 'flex'}
        />
      )}
      <Agenda
        space={space}
        service={service}
        tz={tz}
        view={view}
        firstDay={firstDay}
        start={start}
        end={end}
        now={now}
        calendars={calendars}
        calendar={calendar}
        opened={opened}
        onStep={(by) => go({ ...here, from: dayKey(stepWindow(view, start, by)) })}
        onToday={() => go({ ...here, from: undefined })}
        onView={(next) =>
          // A window that holds today keeps holding it; another keeps the
          // day it was opened on, so month → week → month lands back on
          // the same month even when the week reaches into the one before.
          go({
            ...here,
            view: next === 'week' ? undefined : next,
            from: now >= start && now < end ? undefined : anchor,
          })
        }
        onOpen={(e) => go({ ...here, event: e.key, instance: e.instance })}
        onBack={fixed ? undefined : () => go({ view: place.view, from: place.from })}
        className={previewing ? 'hidden xl:flex' : picked ? 'flex' : 'hidden md:flex'}
      />
      {previewing && (
        <EventPane
          key={opened}
          space={space}
          service={service}
          tz={tz}
          eventKey={place.event!}
          instance={place.instance ?? ''}
          calendar={calendar}
          onClose={() => go(here)}
          onDeleted={() => go(here, 'replace')}
          className="min-w-0 flex-1 xl:w-96 xl:flex-none"
        />
      )}
    </Row>
  )
}

// The calendar's own colour, when it is one; anything else is no colour.
const safeColor = (c?: string) => (c && /^#[0-9a-f]{6}(?:[0-9a-f]{2})?$/i.test(c) ? c : undefined)

function Swatch({ color, className = 'size-2.5 rounded-full' }: { color?: string; className?: string }) {
  const c = safeColor(color)
  return (
    <span
      aria-hidden
      className={`shrink-0 ${c ? '' : 'bg-line-strong'} ${className}`}
      style={c ? { backgroundColor: c } : undefined}
    />
  )
}

// ─── Calendars ─────────────────────────────────────────────────────────

function CalendarList({
  calendars,
  current,
  onPick,
  className,
}: {
  calendars: ReturnType<typeof useCalendars>
  current?: string
  onPick: (key: string) => void
  className: string
}) {
  return (
    <aside
      aria-label="Calendars"
      className={`w-full shrink-0 flex-col overflow-hidden bg-surface/60 md:w-64 ${className}`}
    >
      <div className="flex min-h-11 shrink-0 items-center justify-between gap-2 border-b border-line pr-1.5 pl-3">
        <h2 className="text-sm font-semibold text-ink">Calendars</h2>
        <Toolbar>
          <IconButton
            icon={RefreshCw}
            label="Read the calendars again"
            onClick={() => void calendars.refetch()}
            className={calendars.isFetching ? '[&>svg]:animate-spin' : ''}
          />
        </Toolbar>
      </div>
      <nav aria-label="Calendars" className="min-h-0 flex-1 overflow-y-auto py-1">
        {calendars.error ? (
          <div className="p-2">
            <ErrorNote message={errText(calendars.error, 'The calendars did not answer.')} />
          </div>
        ) : !calendars.data ? (
          <Spinner label="Connecting…" />
        ) : calendars.data.length === 0 ? (
          <p className="px-3 py-6 text-center text-xs leading-relaxed text-muted">
            The service shows you no calendar that holds events.
          </p>
        ) : (
          calendars.data.map((c) => {
            const on = c.key === current
            return (
              <button
                key={c.key}
                type="button"
                onClick={() => onPick(c.key)}
                aria-current={on || undefined}
                title={c.description || c.name}
                className={`flex h-8 w-full items-center gap-2.5 px-3 text-left text-sm transition-colors focus-visible:outline-2 focus-visible:-outline-offset-2 focus-visible:outline-accent-strong ${
                  on ? 'bg-accent-soft text-ink' : 'text-ink-soft hover:bg-line/50 hover:text-ink'
                }`}
              >
                <Swatch color={c.color} />
                <span className="min-w-0 flex-1 truncate">{c.name}</span>
              </button>
            )
          })
        )}
      </nav>
    </aside>
  )
}

// ─── The agenda ────────────────────────────────────────────────────────

function Agenda({
  space,
  service,
  tz,
  view,
  firstDay,
  start,
  end,
  now,
  calendars,
  calendar,
  opened,
  onStep,
  onToday,
  onView,
  onOpen,
  onBack,
  className,
}: {
  space: string
  service: string
  tz: string
  view: CalendarView
  firstDay: number
  start: Date
  end: Date
  now: Date
  calendars: ReturnType<typeof useCalendars>
  calendar?: Calendar
  opened?: string
  onStep: (by: 1 | -1) => void
  onToday: () => void
  onView: (view: CalendarView) => void
  onOpen: (e: EventSummary) => void
  /** Back to the calendars, on a narrow screen; none when there is no list. */
  onBack?: () => void
  className: string
}) {
  const qc = useQueryClient()
  const events = useEvents(space, service, calendar?.key, start, end, tz)
  const unit = view === 'month' ? 'month' : 'week'

  // The windows either side are one click away: read them ahead, once this
  // one is in.
  const loaded = events.data !== undefined
  const calendarKey = calendar?.key
  useEffect(() => {
    if (!loaded || calendarKey === undefined) return
    for (const by of [-1, 1] as const) {
      const w = viewWindow(view, stepWindow(view, start, by), firstDay)
      void qc.prefetchQuery(eventsQuery(space, service, calendarKey, w.start, w.end, tz))
    }
  }, [loaded, calendarKey, view, start, firstDay, qc, space, service, tz])

  const list = events.data?.events
  const days = list ? agendaDays(list, start, end, now) : []
  const count = days.reduce((n, d) => n + d.events.length, 0)

  let body: ReactNode
  if (calendars.error) {
    body = (
      <PaneEmpty icon={CalendarDays} title="No calendars to show">
        The service did not answer — see the note on the left.
      </PaneEmpty>
    )
  } else if (calendars.data?.length === 0) {
    body = (
      <PaneEmpty icon={CalendarDays} title="No calendars">
        The service shows you no calendar that holds events.
      </PaneEmpty>
    )
  } else if (calendars.data && !calendar) {
    body = (
      <PaneEmpty icon={CalendarDays} title="This calendar is gone">
        It was removed or renamed — pick another on the left.
      </PaneEmpty>
    )
  } else if (events.error) {
    body = (
      <div className="p-3">
        <ErrorNote message={errText(events.error, 'Reading the events did not work.')} />
      </div>
    )
  } else if (!list) {
    body = <Spinner label="Reading the events…" />
  } else if (count === 0) {
    body = (
      <PaneEmpty icon={CalendarDays} title={`Nothing on this ${unit}`}>
        {calendar?.name ?? 'The calendar'} has no events between {windowTitle(view, start, end)}.
      </PaneEmpty>
    )
  } else {
    body = (
      <div className="min-h-0 flex-1 overflow-y-auto">
        {days.map((d) => (
          <section key={d.key} aria-label={dayTitle(d.date)} className="border-t border-line first:border-t-0">
            <h3 className="sticky top-0 z-10 flex h-8 items-center gap-2 border-b border-line bg-card px-4 text-xs font-semibold text-ink">
              {dayTitle(d.date)}
              {d.today && (
                <span className="rounded bg-accent-soft px-1.5 py-0.5 text-[10px] leading-none font-medium text-accent-strong">
                  Today
                </span>
              )}
            </h3>
            {d.events.length === 0 ? (
              <p className="px-4 py-2.5 text-xs text-muted">Nothing on today.</p>
            ) : (
              <ul className="divide-y divide-line">
                {d.events.map((e) => (
                  <EventRow
                    key={occurrenceId(e)}
                    event={e}
                    day={d.date}
                    color={calendar?.color}
                    current={occurrenceId(e) === opened}
                    onOpen={onOpen}
                  />
                ))}
              </ul>
            )}
          </section>
        ))}
        {events.data?.truncated && (
          <p className="border-t border-line px-4 py-3 text-xs text-muted">
            Only the first {list.length.toLocaleString()} events of this {unit} — the calendar holds more.
          </p>
        )}
      </div>
    )
  }

  return (
    <section aria-label="Agenda" className={`min-h-0 min-w-0 flex-1 flex-col overflow-hidden ${className}`}>
      <PaneHeader
        className="pl-1.5"
        actions={
          <>
            <Segmented
              label="Show"
              value={view}
              onChange={onView}
              options={[
                { value: 'week', label: 'Week' },
                { value: 'month', label: 'Month' },
              ]}
            />
            <IconButton
              icon={RefreshCw}
              label="Read the events again"
              disabled={!calendar}
              onClick={() => void events.refetch()}
              className={events.isFetching ? '[&>svg]:animate-spin' : ''}
            />
          </>
        }
      >
        <div className="flex min-w-0 items-center gap-0.5">
          {onBack && (
            <IconButton icon={ArrowLeft} label="Back to the calendars" onClick={onBack} className="md:hidden" />
          )}
          <IconButton icon={ChevronLeft} label={`Previous ${unit}`} onClick={() => onStep(-1)} />
          <Button kind="quiet" size="sm" onClick={onToday}>
            Today
          </Button>
          <IconButton icon={ChevronRight} label={`Next ${unit}`} onClick={() => onStep(1)} />
          <h2 className="min-w-0 truncate pl-2 text-sm font-semibold text-ink">{windowTitle(view, start, end)}</h2>
        </div>
      </PaneHeader>
      {body}
    </section>
  )
}

function EventRow({
  event: e,
  day,
  color,
  current,
  onOpen,
}: {
  event: EventSummary
  day: Date
  color?: string
  current: boolean
  onOpen: (e: EventSummary) => void
}) {
  const time = rowTime(e, day)
  const cancelled = e.status === 'cancelled'
  return (
    <li>
      <button
        type="button"
        onClick={() => onOpen(e)}
        aria-current={current || undefined}
        className={`flex w-full items-start gap-3 px-4 py-2 text-left transition-colors focus-visible:outline-2 focus-visible:-outline-offset-2 focus-visible:outline-accent-strong ${
          current ? 'bg-accent-soft' : 'hover:bg-line/40'
        }`}
      >
        <span className="w-20 shrink-0 text-xs leading-5 text-ink-soft tabular-nums">
          {time.from}
          {time.to && <span className="block text-muted">{time.to}</span>}
        </span>
        <Swatch color={color} className={`mt-0.5 h-4 w-1 rounded-full ${e.free ? 'opacity-40' : ''}`} />
        <span className="min-w-0 flex-1">
          <span className="flex min-w-0 items-center gap-1.5">
            <span className={`truncate text-sm ${cancelled ? 'text-muted line-through' : 'text-ink'}`}>
              {e.summary || 'No title'}
            </span>
            {e.recurring && (
              <>
                <Repeat className="size-3 shrink-0 text-muted" aria-hidden />
                <span className="sr-only">(repeats)</span>
              </>
            )}
            {e.status === 'tentative' && <span className="shrink-0 text-xs text-muted">tentative</span>}
          </span>
          {e.location && <span className="block truncate text-xs text-muted">{e.location}</span>}
        </span>
      </button>
    </li>
  )
}

// ─── The event ─────────────────────────────────────────────────────────

function EventPane({
  space,
  service,
  tz,
  eventKey,
  instance,
  calendar,
  onClose,
  onDeleted,
  className = '',
}: {
  space: string
  service: string
  tz: string
  eventKey: string
  instance: string
  calendar?: Calendar
  onClose: () => void
  onDeleted: () => void
  className?: string
}) {
  const detail = useEvent(space, service, eventKey, instance, tz)
  const [deleting, setDeleting] = useState(false)
  const gone = (detail.error as { status?: number } | null)?.status === 404
  const e = detail.data
  const series = !!e && (e.recurring || !!e.rule)

  return (
    <aside aria-label="Event" className={`flex min-h-0 flex-col overflow-hidden ${className}`}>
      <header className="flex min-h-11 shrink-0 items-center gap-2 border-b border-line py-1 pr-1.5 pl-3">
        <Swatch color={calendar?.color} />
        <h3 className="min-w-0 flex-1 truncate text-sm font-semibold text-ink" title={e?.summary || undefined}>
          {e ? e.summary || 'No title' : <span className="text-muted">Event</span>}
        </h3>
        <IconButton icon={X} label="Close the event" onClick={onClose} />
      </header>
      <div className="min-h-0 flex-1 overflow-y-auto">
        {gone ? (
          <p className="p-4 text-sm text-muted">
            This event is gone — it was deleted or moved, perhaps from another app that syncs the calendar.
          </p>
        ) : detail.error ? (
          <div className="p-3">
            <ErrorNote message={errText(detail.error, 'Reading the event did not work.')} />
          </div>
        ) : !e ? (
          <Spinner label="Reading the event…" />
        ) : (
          <EventFacts event={e} calendar={calendar} />
        )}
      </div>
      <footer className="flex shrink-0 items-center justify-between gap-2 border-t border-line py-2 pr-1.5 pl-3">
        <Button
          size="sm"
          icon={Download}
          disabled={!e}
          onClick={() =>
            e && saveBlob(new Blob([e.data], { type: 'text/calendar' }), downloadName(e.summary, 'event', 'ics'))
          }
        >
          Download
        </Button>
        <IconButton
          icon={Trash2}
          kind="danger"
          label={series ? 'Delete the series…' : 'Delete the event…'}
          disabled={!e}
          onClick={() => setDeleting(true)}
        />
      </footer>
      {deleting && e && (
        <DeleteEvent
          space={space}
          service={service}
          event={e}
          calendarName={calendar?.name ?? service}
          onClose={() => setDeleting(false)}
          onDeleted={() => {
            setDeleting(false)
            onDeleted()
          }}
        />
      )}
    </aside>
  )
}

const attendeeWords: Record<AttendeeStatus, string> = {
  'needs-action': 'no answer',
  accepted: 'accepted',
  declined: 'declined',
  tentative: 'maybe',
  delegated: 'delegated',
}

const isWeb = (v: string) => /^https?:\/\/\S+$/i.test(v.trim())

function EventFacts({ event: e, calendar }: { event: EventDetail; calendar?: Calendar }) {
  const series = e.recurring || !!e.rule
  const rule = e.rule ? describeRule(e.rule) : null
  const visibility = e.class && e.class !== 'public' ? e.class : ''
  const marks = [
    e.status === 'cancelled' && 'cancelled',
    e.status === 'tentative' && 'tentative',
    e.free && 'shows as free',
    visibility,
  ].filter(Boolean) as string[]
  const stamp = (iso?: string) => {
    const d = iso ? new Date(iso) : null
    return d && !Number.isNaN(d.getTime())
      ? new Intl.DateTimeFormat(undefined, { dateStyle: 'medium', timeStyle: 'short' }).format(d)
      : ''
  }
  const created = stamp(e.created)
  const modified = stamp(e.modified)

  return (
    <div className="space-y-4 p-4">
      <div>
        <p className="text-sm leading-relaxed text-ink">{whenText(e)}</p>
        {marks.length > 0 && (
          <div className="mt-1.5 flex flex-wrap gap-1">
            {marks.map((m) => (
              <Chip key={m}>{m}</Chip>
            ))}
          </div>
        )}
      </div>
      {series && (
        <Fact label="Repeats">
          {!e.rule ? (
            'One occurrence of a series'
          ) : rule ? (
            rule
          ) : (
            <span className="font-mono text-xs break-all">{e.rule}</span>
          )}
        </Fact>
      )}
      {e.location && <Fact label="Where">{isWeb(e.location) ? <WebLink href={e.location} /> : e.location}</Fact>}
      {e.organizer && (
        <Fact label="Organiser">
          <PersonLine person={e.organizer} />
        </Fact>
      )}
      {e.attendees.length > 0 && (
        <Fact label={`Attendees · ${e.attendees.length}`}>
          <ul className="space-y-1">
            {e.attendees.map((a, i) => (
              <li key={i} className="flex min-w-0 items-center gap-2">
                <PersonLine person={a} />
                {a.status && (
                  <Chip tone={a.status === 'accepted' ? 'success' : 'neutral'}>{attendeeWords[a.status]}</Chip>
                )}
              </li>
            ))}
          </ul>
        </Fact>
      )}
      {e.description && (
        <Fact label="Description">
          <p className="whitespace-pre-wrap text-ink-soft">{e.description}</p>
        </Fact>
      )}
      {e.url && <Fact label="Link">{isWeb(e.url) ? <WebLink href={e.url} /> : e.url}</Fact>}
      {e.categories.length > 0 && (
        <Fact label="Categories">
          <span className="flex flex-wrap gap-1 pt-0.5">
            {e.categories.map((c) => (
              <Chip key={c}>{c}</Chip>
            ))}
          </span>
        </Fact>
      )}
      {calendar && (
        <Fact label="Calendar">
          <span className="flex items-center gap-2">
            <Swatch color={calendar.color} />
            {calendar.name}
          </span>
        </Fact>
      )}
      {(created || modified) && (
        <p className="border-t border-line pt-3 text-xs text-muted">
          {[created && `Created ${created}`, modified && `changed ${modified}`].filter(Boolean).join(' · ')}
        </p>
      )}
    </div>
  )
}

function Fact({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div className="min-w-0">
      <Eyebrow>{label}</Eyebrow>
      <div className="mt-1 text-sm break-words text-ink">{children}</div>
    </div>
  )
}

function PersonLine({ person: p }: { person: Person }) {
  const shown = p.name || p.email || 'Someone'
  return (
    <span className="min-w-0 truncate" title={p.email && p.name ? `${p.name} <${p.email}>` : shown}>
      {shown}
      {p.name && p.email && <span className="text-muted"> · {p.email}</span>}
    </span>
  )
}

function WebLink({ href }: { href: string }) {
  return (
    <a
      href={href.trim()}
      target="_blank"
      rel="noreferrer"
      className="break-all text-ink underline decoration-line-strong underline-offset-2 transition-colors hover:text-accent-strong hover:decoration-accent-strong"
    >
      {href.trim().replace(/^https?:\/\//i, '')}
    </a>
  )
}

function DeleteEvent({
  space,
  service,
  event,
  calendarName,
  onClose,
  onDeleted,
}: {
  space: string
  service: string
  event: EventDetail
  calendarName: string
  onClose: () => void
  onDeleted: () => void
}) {
  const remove = useDeleteEvent(space, service)
  const busy = remove.isPending
  const series = event.recurring || !!event.rule
  const name = event.summary || 'this event'
  return (
    <Modal onClose={() => !busy && onClose()} size="sm">
      <ModalHeader
        eyebrow={calendarName}
        title={series ? `Delete every “${name}”?` : `Delete “${name}”?`}
        onClose={onClose}
        closeDisabled={busy}
      />
      <ModalBody>
        <div className="space-y-2 text-sm leading-relaxed text-ink-soft">
          {series ? (
            <p>
              It repeats, and the calendar keeps the series as one: deleting it deletes every occurrence, past and
              future — not only this one.
            </p>
          ) : (
            <p>The event leaves the calendar, and every phone and app that syncs it. It does not come back.</p>
          )}
          {event.attendees.length > 0 && <p>If you organise it, the service may send its attendees a cancellation.</p>}
        </div>
        {remove.error && (
          <div className="mt-3">
            <ErrorNote message={mutationError(remove, 'Deleting did not work.')} />
          </div>
        )}
      </ModalBody>
      <ModalFooter>
        <Button kind="quiet" onClick={onClose} disabled={busy}>
          Keep it
        </Button>
        <Button
          kind="danger"
          icon={Trash2}
          disabled={busy}
          onClick={() => remove.mutate({ key: event.key, etag: event.etag }, { onSuccess: onDeleted })}
        >
          {busy ? 'Deleting…' : series ? 'Delete the series' : 'Delete'}
        </Button>
      </ModalFooter>
    </Modal>
  )
}

// The calendar console's date work, kept pure: which days a view shows, on
// which of them an event appears, and how its times and repeats read. Days
// are the viewer's local days throughout. An all-day date ("2026-11-20") is
// a local day too — never `new Date('2026-11-20')`, which is midnight in UTC
// and the day before west of Greenwich.
import type { EventSummary } from './calendarApi'

export type CalendarView = 'week' | 'month'

const dayPattern = /^(\d{4})-(\d{2})-(\d{2})$/

/** A "YYYY-MM-DD" as local midnight, or null when it is not one. */
export function parseDay(day: string): Date | null {
  const m = dayPattern.exec(day)
  if (!m) return null
  const d = new Date(Number(m[1]), Number(m[2]) - 1, Number(m[3]))
  // 2026-02-31 is not a day; Date would roll it into March.
  return d.getMonth() === Number(m[2]) - 1 ? d : null
}

const pad = (n: number) => String(n).padStart(2, '0')

/** A date's local day, as "YYYY-MM-DD". */
export const dayKey = (d: Date) => `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}`

/** n days on, at local midnight — whatever the clocks did in between. */
export const addDays = (d: Date, n: number) => new Date(d.getFullYear(), d.getMonth(), d.getDate() + n)

export const startOfDay = (d: Date) => new Date(d.getFullYear(), d.getMonth(), d.getDate())

/**
 * The day a week starts on where the viewer is, as Date#getDay counts
 * (0 is Sunday). Intl names it (1 is Monday … 7 is Sunday); Monday when it
 * cannot say.
 */
export function firstDayOfWeek(locale?: string): number {
  try {
    const l = new Intl.Locale(
      locale ?? (typeof navigator === 'undefined' ? 'en' : navigator.language),
    ) as Intl.Locale & {
      getWeekInfo?: () => { firstDay: number }
      weekInfo?: { firstDay: number }
    }
    const first = l.getWeekInfo?.().firstDay ?? l.weekInfo?.firstDay
    if (first && first >= 1 && first <= 7) return first % 7
  } catch {
    // An unknown locale tag: fall through.
  }
  return 1
}

/** The days a view shows around a date: start included, end excluded, both local midnights. */
export function viewWindow(view: CalendarView, at: Date, firstDay: number): { start: Date; end: Date } {
  if (view === 'month') {
    const start = new Date(at.getFullYear(), at.getMonth(), 1)
    return { start, end: new Date(at.getFullYear(), at.getMonth() + 1, 1) }
  }
  const start = addDays(startOfDay(at), -((at.getDay() - firstDay + 7) % 7))
  return { start, end: addDays(start, 7) }
}

/** The window one step back or on. */
export function stepWindow(view: CalendarView, start: Date, by: 1 | -1): Date {
  return view === 'month' ? new Date(start.getFullYear(), start.getMonth() + by, 1) : addDays(start, 7 * by)
}

/** Where an event starts and ends, as instants. An all-day end is the exclusive day after. */
export function eventSpan(e: Pick<EventSummary, 'start' | 'end' | 'allDay'>): { start: Date; end: Date } | null {
  if (e.allDay) {
    const start = parseDay(e.start)
    if (!start) return null
    const end = parseDay(e.end)
    return { start, end: end && end > start ? end : addDays(start, 1) }
  }
  const start = new Date(e.start)
  if (Number.isNaN(start.getTime())) return null
  const end = new Date(e.end)
  return { start, end: Number.isNaN(end.getTime()) || end < start ? start : end }
}

export type AgendaDay<E> = { key: string; date: Date; today: boolean; events: E[] }

/**
 * The days of a window that have something on — and today, when it is in
 * the window — each with its events: the all-day ones first, then by start.
 * An all-day event shows on every day it covers; a timed one on the day it
 * starts, or the window's first day when it began before.
 */
export function agendaDays<E extends Pick<EventSummary, 'start' | 'end' | 'allDay' | 'summary'>>(
  events: E[],
  start: Date,
  end: Date,
  now: Date,
): AgendaDay<E>[] {
  const days = new Map<string, AgendaDay<E>>()
  const day = (date: Date) => {
    const key = dayKey(date)
    let d = days.get(key)
    if (!d) {
      d = { key, date: startOfDay(date), today: key === dayKey(now), events: [] }
      days.set(key, d)
    }
    return d
  }
  for (const e of events) {
    const span = eventSpan(e)
    if (!span) continue
    // A moment (start = end) belongs to the window it falls in, its very
    // start included; anything longer, to every window it overlaps.
    const moment = span.end.getTime() === span.start.getTime()
    if (moment ? span.start < start || span.start >= end : span.end <= start || span.start >= end) continue
    if (e.allDay) {
      for (let d = span.start < start ? start : span.start; d < span.end && d < end; d = addDays(d, 1)) {
        day(d).events.push(e)
      }
    } else {
      day(span.start < start ? start : span.start).events.push(e)
    }
  }
  if (now >= start && now < end) day(now)
  const startOf = (e: E) => eventSpan(e)?.start.getTime() ?? 0
  return [...days.values()]
    .sort((a, b) => a.date.getTime() - b.date.getTime())
    .map((d) => ({
      ...d,
      events: d.events.sort(
        (a, b) =>
          Number(b.allDay) - Number(a.allDay) ||
          startOf(a) - startOf(b) ||
          a.summary.localeCompare(b.summary, undefined, { numeric: true }),
      ),
    }))
}

// ─── Words ─────────────────────────────────────────────────────────────

/** "16 – 22 Nov 2026" for a week, "November 2026" for a month. */
export function windowTitle(view: CalendarView, start: Date, end: Date, locale?: string): string {
  if (view === 'month') return new Intl.DateTimeFormat(locale, { month: 'long', year: 'numeric' }).format(start)
  return new Intl.DateTimeFormat(locale, { day: 'numeric', month: 'short', year: 'numeric' }).formatRange(
    start,
    addDays(end, -1),
  )
}

/** A day's heading: "Friday 20 November". */
export const dayTitle = (d: Date, locale?: string) =>
  new Intl.DateTimeFormat(locale, { weekday: 'long', day: 'numeric', month: 'long' }).format(d)

/**
 * An agenda row's time, seen from the day it is listed on: "09:00 – 10:30",
 * "22:00 – Sat 02:00" when it ends on another day, "until 10:00" when it
 * began before, "All day".
 */
export function rowTime(
  e: Pick<EventSummary, 'start' | 'end' | 'allDay'>,
  day: Date,
  locale?: string,
): { from: string; to: string } {
  if (e.allDay) return { from: 'All day', to: '' }
  const span = eventSpan(e)
  if (!span) return { from: '', to: '' }
  const time = new Intl.DateTimeFormat(locale, { hour: '2-digit', minute: '2-digit' })
  const dayTime = new Intl.DateTimeFormat(locale, { weekday: 'short', hour: '2-digit', minute: '2-digit' })
  const dayStart = startOfDay(day)
  const dayEnd = addDays(dayStart, 1)
  const began = span.start < dayStart
  const sameMoment = span.end.getTime() === span.start.getTime()
  const from = began ? 'until' : time.format(span.start)
  const to = sameMoment ? '' : span.end <= dayEnd ? time.format(span.end) : dayTime.format(span.end)
  return { from, to }
}

/** The detail's "when", in full: "Friday, 20 November 2026, 09:00–10:30". */
export function whenText(e: Pick<EventSummary, 'start' | 'end' | 'allDay'>, locale?: string): string {
  const span = eventSpan(e)
  if (!span) return e.start
  if (e.allDay) {
    const f = new Intl.DateTimeFormat(locale, { dateStyle: 'full' })
    const last = addDays(span.end, -1)
    return dayKey(last) === dayKey(span.start) ? f.format(span.start) : f.formatRange(span.start, last)
  }
  const f = new Intl.DateTimeFormat(locale, { dateStyle: 'full', timeStyle: 'short' })
  return span.end.getTime() === span.start.getTime() ? f.format(span.start) : f.formatRange(span.start, span.end)
}

const weekdays: Record<string, string> = {
  MO: 'Monday',
  TU: 'Tuesday',
  WE: 'Wednesday',
  TH: 'Thursday',
  FR: 'Friday',
  SA: 'Saturday',
  SU: 'Sunday',
}
const ordinals: Record<string, string> = {
  '1': 'first',
  '2': 'second',
  '3': 'third',
  '4': 'fourth',
  '5': 'fifth',
  '-1': 'last',
  '-2': 'second to last',
}
const units: Record<string, string> = { DAILY: 'day', WEEKLY: 'week', MONTHLY: 'month', YEARLY: 'year' }

const list = (words: string[]) =>
  words.length < 2 ? (words[0] ?? '') : `${words.slice(0, -1).join(', ')} and ${words[words.length - 1]}`

/** An RRULE's UNTIL as a date to read: "20261231", "20261231T225959Z". */
function untilDate(raw: string): Date | null {
  const m = /^(\d{4})(\d{2})(\d{2})(?:T(\d{2})(\d{2})(\d{2})(Z)?)?$/.exec(raw)
  if (!m) return null
  const [y, mo, d] = [Number(m[1]), Number(m[2]) - 1, Number(m[3])]
  if (!m[4]) return new Date(y, mo, d)
  const [h, mi, s] = [Number(m[4]), Number(m[5]), Number(m[6])]
  return m[7] ? new Date(Date.UTC(y, mo, d, h, mi, s)) : new Date(y, mo, d, h, mi, s)
}

/**
 * A repeat rule in plain words — "Every 2 weeks on Monday and Thursday,
 * 10 times" — for the rules people make by hand. Anything finer (positions
 * in a set, hours, week numbers) is null: the rule is shown as written.
 */
export function describeRule(rule: string, locale?: string): string | null {
  const parts = new Map<string, string>()
  for (const part of rule
    .trim()
    .replace(/^RRULE:/i, '')
    .split(';')) {
    if (!part) continue
    const [name, value = ''] = part.split('=')
    parts.set(name.toUpperCase(), value.toUpperCase())
  }
  const freq = parts.get('FREQ') ?? ''
  const unit = units[freq]
  const known = new Set(['FREQ', 'INTERVAL', 'COUNT', 'UNTIL', 'BYDAY', 'BYMONTHDAY', 'WKST'])
  if (!unit || [...parts.keys()].some((k) => !known.has(k))) return null
  const interval = Number(parts.get('INTERVAL') ?? '1')
  if (!Number.isInteger(interval) || interval < 1) return null

  let text = interval === 1 ? `Every ${unit}` : `Every ${interval} ${unit}s`
  const byDay = parts.get('BYDAY')?.split(',').filter(Boolean) ?? []
  const byMonthDay = parts.get('BYMONTHDAY')?.split(',').filter(Boolean) ?? []
  if (byDay.length > 0 && byMonthDay.length > 0) return null
  if (byDay.length > 0) {
    const days: { n: string; day: string }[] = []
    for (const d of byDay) {
      const m = /^([+-]?\d)?(MO|TU|WE|TH|FR|SA|SU)$/.exec(d)
      if (!m) return null
      days.push({ n: (m[1] ?? '').replace('+', ''), day: m[2] })
    }
    const numbered = days.some((d) => d.n)
    if (numbered) {
      if (freq !== 'MONTHLY' || days.some((d) => !ordinals[d.n])) return null
      text += ` on the ${list(days.map((d) => `${ordinals[d.n]} ${weekdays[d.day]}`))}`
    } else if (freq !== 'WEEKLY') {
      // Un-numbered weekdays in a month or a year are every such day in
      // it, not one: "every Monday", which a weekly rule says better.
      return null
    } else if (freq === 'WEEKLY' && interval === 1 && days.length === 5 && !days.some((d) => /SA|SU/.test(d.day))) {
      text = 'Every weekday'
    } else {
      text += ` on ${list(days.map((d) => weekdays[d.day]))}`
    }
  }
  if (byMonthDay.length > 0) {
    if (freq !== 'MONTHLY' || byMonthDay.some((d) => !/^-?\d{1,2}$/.test(d))) return null
    const named = byMonthDay.map((d) => (d === '-1' ? 'the last day' : Number(d) < 0 ? null : `day ${Number(d)}`))
    if (named.some((d) => d === null)) return null
    text += ` on ${list(named as string[])}`
  }
  const count = parts.get('COUNT')
  const until = parts.get('UNTIL')
  if (count) {
    const n = Number(count)
    if (!Number.isInteger(n) || n < 1) return null
    text += n === 1 ? ', once' : `, ${n} times`
  } else if (until) {
    const d = untilDate(until)
    if (!d) return null
    text += `, until ${new Intl.DateTimeFormat(locale, { dateStyle: 'medium' }).format(d)}`
  }
  return text
}

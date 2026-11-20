import assert from 'node:assert/strict'
import { test } from 'node:test'
import {
  addDays,
  agendaDays,
  dayKey,
  describeRule,
  eventSpan,
  firstDayOfWeek,
  parseDay,
  rowTime,
  stepWindow,
  viewWindow,
  whenText,
  windowTitle,
} from '../src/calendarDates.ts'

// Every instant here is built from local parts, so the tests mean the same
// in any zone the machine runs in.
const at = (y, m, d, h = 0, min = 0) => new Date(y, m - 1, d, h, min)
const timed = (summary, start, end) => ({
  summary,
  start: start.toISOString(),
  end: end.toISOString(),
  allDay: false,
})
const allDay = (summary, start, end) => ({ summary, start, end, allDay: true })
// ICU spaces its ranges with thin and narrow spaces; the words are what count.
const plain = (s) => s.replace(/\s+/g, ' ')

test('all-day dates are local days, and impossible ones are not days', () => {
  const d = parseDay('2026-11-20')
  assert.equal(d.getFullYear(), 2026)
  assert.equal(d.getMonth(), 10)
  assert.equal(d.getDate(), 20)
  assert.equal(d.getHours(), 0)
  assert.equal(dayKey(d), '2026-11-20')
  assert.equal(parseDay('2026-02-31'), null)
  assert.equal(parseDay('20261120'), null)
  assert.equal(dayKey(addDays(at(2026, 3, 28), 2)), '2026-03-30')
})

test('a week starts on the locale’s first day, a month on its first', () => {
  assert.equal(firstDayOfWeek('de-CH'), 1)
  assert.equal(firstDayOfWeek('en-US'), 0)
  assert.equal(firstDayOfWeek('not a locale!'), 1)

  const friday = at(2026, 11, 20, 15)
  let w = viewWindow('week', friday, 1)
  assert.equal(dayKey(w.start), '2026-11-16')
  assert.equal(dayKey(w.end), '2026-11-23')
  w = viewWindow('week', friday, 0)
  assert.equal(dayKey(w.start), '2026-11-15')
  w = viewWindow('week', at(2026, 11, 16), 1)
  assert.equal(dayKey(w.start), '2026-11-16')
  w = viewWindow('month', friday, 1)
  assert.equal(dayKey(w.start), '2026-11-01')
  assert.equal(dayKey(w.end), '2026-12-01')

  assert.equal(dayKey(stepWindow('week', at(2026, 11, 16), 1)), '2026-11-23')
  assert.equal(dayKey(stepWindow('month', at(2026, 12, 1), 1)), '2027-01-01')
  assert.equal(dayKey(stepWindow('month', at(2026, 1, 1), -1)), '2025-12-01')
})

test('an all-day end is exclusive; one without a usable end lasts its day', () => {
  const span = eventSpan(allDay('trip', '2026-11-20', '2026-11-23'))
  assert.equal(dayKey(span.start), '2026-11-20')
  assert.equal(dayKey(span.end), '2026-11-23')
  assert.equal(dayKey(eventSpan(allDay('x', '2026-11-20', '2026-11-20')).end), '2026-11-21')
  assert.equal(eventSpan(allDay('x', 'soon', '')), null)
})

test('the agenda lists the days with something on, and today', () => {
  const { start, end } = viewWindow('week', at(2026, 11, 18), 1) // 16 – 23 Nov
  const now = at(2026, 11, 18, 12)
  const days = agendaDays(
    [
      timed('standup', at(2026, 11, 20, 9), at(2026, 11, 20, 9, 15)),
      allDay('trip', '2026-11-20', '2026-11-23'),
      timed('late', at(2026, 11, 20, 23), at(2026, 11, 21, 1)),
      timed('early', at(2026, 11, 20, 8), at(2026, 11, 20, 9)),
      // Began the week before, ends inside: shown on the first day.
      timed('carry-over', at(2026, 11, 15, 22), at(2026, 11, 16, 2)),
      // Outside the window altogether.
      timed('next week', at(2026, 11, 23, 9), at(2026, 11, 23, 10)),
      allDay('last week', '2026-11-14', '2026-11-16'),
      // A moment at the window's very start belongs to it.
      timed('midnight', at(2026, 11, 16), at(2026, 11, 16)),
    ],
    start,
    end,
    now,
  )
  assert.deepEqual(
    days.map((d) => [d.key, d.today, d.events.map((e) => e.summary)]),
    [
      ['2026-11-16', false, ['carry-over', 'midnight']],
      ['2026-11-18', true, []],
      ['2026-11-20', false, ['trip', 'early', 'standup', 'late']],
      ['2026-11-21', false, ['trip']],
      ['2026-11-22', false, ['trip']],
    ],
  )
  // A week that is not this one has no today to add.
  const later = viewWindow('week', at(2026, 12, 1), 1)
  assert.deepEqual(agendaDays([], later.start, later.end, now), [])
})

test('a row’s time reads from the day it is listed on', () => {
  const day = at(2026, 11, 20)
  assert.deepEqual(rowTime(timed('a', at(2026, 11, 20, 9), at(2026, 11, 20, 10, 30)), day, 'en-GB'), {
    from: '09:00',
    to: '10:30',
  })
  assert.deepEqual(rowTime(timed('a', at(2026, 11, 20, 22), at(2026, 11, 21, 2)), day, 'en-GB'), {
    from: '22:00',
    to: 'Sat 02:00',
  })
  // Ending at the next midnight is ending today.
  assert.deepEqual(rowTime(timed('a', at(2026, 11, 20, 22), at(2026, 11, 21)), day, 'en-GB'), {
    from: '22:00',
    to: '00:00',
  })
  assert.deepEqual(rowTime(timed('a', at(2026, 11, 19, 22), at(2026, 11, 20, 2)), day, 'en-GB'), {
    from: 'until',
    to: '02:00',
  })
  assert.deepEqual(rowTime(timed('a', at(2026, 11, 20, 9), at(2026, 11, 20, 9)), day, 'en-GB'), {
    from: '09:00',
    to: '',
  })
  assert.deepEqual(rowTime(allDay('a', '2026-11-20', '2026-11-21'), day, 'en-GB'), { from: 'All day', to: '' })
})

test('titles and the detail’s when read in full', () => {
  const w = viewWindow('week', at(2026, 11, 18), 1)
  assert.equal(plain(windowTitle('week', w.start, w.end, 'en-GB')), '16 – 22 Nov 2026')
  const m = viewWindow('month', at(2026, 11, 18), 1)
  assert.equal(plain(windowTitle('month', m.start, m.end, 'en-GB')), 'November 2026')

  assert.equal(plain(whenText(allDay('a', '2026-11-20', '2026-11-21'), 'en-GB')), 'Friday, 20 November 2026')
  assert.match(whenText(allDay('a', '2026-11-20', '2026-11-23'), 'en-GB'), /20.*22 November 2026/)
  assert.match(
    whenText(timed('a', at(2026, 11, 20, 9), at(2026, 11, 20, 10, 30)), 'en-GB'),
    /20 November 2026.*09:00.*10:30/,
  )
})

test('repeat rules people make read as words; finer ones stay as written', () => {
  const cases = [
    ['FREQ=DAILY', 'Every day'],
    ['FREQ=DAILY;INTERVAL=3', 'Every 3 days'],
    ['FREQ=WEEKLY;BYDAY=MO', 'Every week on Monday'],
    ['FREQ=WEEKLY;INTERVAL=2;BYDAY=MO,TH', 'Every 2 weeks on Monday and Thursday'],
    ['FREQ=WEEKLY;BYDAY=MO,TU,WE,TH,FR', 'Every weekday'],
    ['FREQ=WEEKLY;BYDAY=MO,WE,FR;WKST=SU', 'Every week on Monday, Wednesday and Friday'],
    ['FREQ=MONTHLY;BYDAY=2TU', 'Every month on the second Tuesday'],
    ['FREQ=MONTHLY;BYDAY=-1FR', 'Every month on the last Friday'],
    ['FREQ=MONTHLY;BYMONTHDAY=15', 'Every month on day 15'],
    ['FREQ=MONTHLY;BYMONTHDAY=-1', 'Every month on the last day'],
    ['FREQ=YEARLY', 'Every year'],
    ['FREQ=WEEKLY;COUNT=10', 'Every week, 10 times'],
    ['RRULE:FREQ=DAILY;COUNT=1', 'Every day, once'],
    ['FREQ=WEEKLY;UNTIL=20261231', 'Every week, until 31 Dec 2026'],
  ]
  for (const [rule, words] of cases) assert.equal(describeRule(rule, 'en-GB'), words, rule)
  for (const rule of [
    'FREQ=MONTHLY;BYDAY=MO,TU;BYSETPOS=1',
    'FREQ=YEARLY;BYMONTH=3;BYDAY=-1SU',
    'FREQ=HOURLY',
    'FREQ=WEEKLY;BYDAY=2MO',
    'FREQ=MONTHLY;BYDAY=MO',
    'FREQ=WEEKLY;INTERVAL=0',
    'BYDAY=MO',
    'FREQ=WEEKLY;UNTIL=tomorrow',
  ]) {
    assert.equal(describeRule(rule, 'en-GB'), null, rule)
  }
})

package pim

import (
	"slices"
	"strconv"
	"strings"
	"time"
)

// rule is the part of RFC 5545's RRULE that calendars write for people:
// daily, weekly, monthly and yearly series, every so often, so many times
// or until a day, on given weekdays, days of the month and months. A rule
// with anything else (BYSETPOS, BYWEEKNO, hourly…) is not read: its series
// shows its first occurrence only, which a CalDAV service that expands
// series never lets happen.
type rule struct {
	freq       string
	interval   int
	count      int
	until      time.Time // zero: none
	byDay      []weekdayNum
	byMonthDay []int
	byMonth    []time.Month
	wkst       time.Weekday
}

// weekdayNum is a BYDAY entry: MO, or 2MO (the second Monday), -1FR (the
// last Friday).
type weekdayNum struct {
	n   int
	day time.Weekday
}

var allMonths = []time.Month{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}

var weekdays = map[string]time.Weekday{
	"SU": time.Sunday, "MO": time.Monday, "TU": time.Tuesday, "WE": time.Wednesday,
	"TH": time.Thursday, "FR": time.Friday, "SA": time.Saturday,
}

func parseRule(v string, start When) (rule, bool) {
	r := rule{interval: 1, wkst: time.Monday}
	for part := range strings.SplitSeq(v, ";") {
		key, val, _ := strings.Cut(part, "=")
		key, val = strings.ToUpper(trim(key)), strings.ToUpper(trim(val))
		switch key {
		case "FREQ":
			r.freq = val
		case "INTERVAL":
			n, err := strconv.Atoi(val)
			if err != nil || n < 1 {
				return rule{}, false
			}
			r.interval = n
		case "COUNT":
			n, err := strconv.Atoi(val)
			if err != nil || n < 1 {
				return rule{}, false
			}
			r.count = n
		case "UNTIL":
			until, ok := parseUntil(val, start)
			if !ok {
				return rule{}, false
			}
			r.until = until
		case "BYDAY":
			for d := range strings.SplitSeq(val, ",") {
				if len(d) < 2 {
					return rule{}, false
				}
				day, ok := weekdays[d[len(d)-2:]]
				if !ok {
					return rule{}, false
				}
				n := 0
				if num := d[:len(d)-2]; num != "" {
					var err error
					if n, err = strconv.Atoi(num); err != nil || n == 0 || n < -53 || n > 53 {
						return rule{}, false
					}
				}
				r.byDay = append(r.byDay, weekdayNum{n, day})
			}
		case "BYMONTHDAY":
			for d := range strings.SplitSeq(val, ",") {
				n, err := strconv.Atoi(d)
				if err != nil || n == 0 || n < -31 || n > 31 {
					return rule{}, false
				}
				r.byMonthDay = append(r.byMonthDay, n)
			}
		case "BYMONTH":
			for m := range strings.SplitSeq(val, ",") {
				n, err := strconv.Atoi(m)
				if err != nil || n < 1 || n > 12 {
					return rule{}, false
				}
				r.byMonth = append(r.byMonth, time.Month(n))
			}
		case "WKST":
			day, ok := weekdays[val]
			if !ok {
				return rule{}, false
			}
			r.wkst = day
		case "":
		default:
			return rule{}, false
		}
	}
	switch r.freq {
	case "DAILY", "WEEKLY":
		// Ordinals only mean something within a month or a year.
		for _, d := range r.byDay {
			if d.n != 0 {
				return rule{}, false
			}
		}
	case "MONTHLY":
		for _, d := range r.byDay {
			if d.n < -5 || d.n > 5 {
				return rule{}, false
			}
		}
	case "YEARLY":
		// Weekdays of the year rather than of its months are not read.
		if len(r.byDay) > 0 && len(r.byMonth) == 0 {
			return rule{}, false
		}
	default:
		return rule{}, false
	}
	return r, true
}

// parseUntil reads UNTIL: an instant in UTC, a date — the whole of that
// day — or a time without a zone, which is in the start's zone (a floating
// series is floating to the end).
func parseUntil(v string, start When) (time.Time, bool) {
	loc := start.Time.Location()
	switch {
	case len(v) == 8:
		t, err := time.Parse("20060102", v)
		if err != nil || start.Date {
			return t, err == nil
		}
		return time.Date(t.Year(), t.Month(), t.Day(), 23, 59, 59, 0, loc), true
	case strings.HasSuffix(v, "Z"):
		t, err := time.Parse("20060102T150405Z", v)
		return t, err == nil
	default:
		t, err := time.ParseInLocation("20060102T150405", v, loc)
		return t, err == nil
	}
}

// occurrences are the series' starts in [after, before): DTSTART first —
// it always counts, matching the rule or not — then what the rule makes,
// counted from DTSTART so COUNT holds, at most max of them.
func (r rule) occurrences(start When, after, before time.Time, max int) []When {
	t0 := start.Time
	loc := t0.Location()
	h, mi, s := t0.Clock()
	at := func(y int, m time.Month, d int) time.Time {
		if start.Date {
			return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
		}
		return time.Date(y, m, d, h, mi, s, 0, loc)
	}
	var out []When
	count := 0
	// emit takes the next start in order; false ends the series.
	emit := func(t time.Time) bool {
		if !r.until.IsZero() && t.After(r.until) || !t.Before(before) {
			return false
		}
		count++
		if r.count > 0 && count > r.count {
			return false
		}
		if !t.Before(after) {
			out = append(out, When{Time: t, Date: start.Date})
		}
		return len(out) < max
	}
	if !emit(t0) {
		return out
	}
	y0, m0, d0 := t0.Date()
	// A period whose first day is past the window or UNTIL can hold no
	// more starts; the cap guards a rule that never matches.
	over := func(first time.Time) bool {
		return !first.Before(before) || !r.until.IsZero() && first.After(r.until)
	}
	for i := 0; i < 100000; i++ {
		var first time.Time
		var days []time.Time
		switch r.freq {
		case "DAILY":
			first = at(y0, m0, d0+i*r.interval)
			if r.matchesDay(first) {
				days = []time.Time{first}
			}
		case "WEEKLY":
			offset := (int(t0.Weekday()) - int(r.wkst) + 7) % 7
			first = at(y0, m0, d0-offset+7*i*r.interval)
			wanted := []time.Weekday{t0.Weekday()}
			if len(r.byDay) > 0 {
				wanted = nil
				for _, d := range r.byDay {
					wanted = append(wanted, d.day)
				}
			}
			for _, wd := range wanted {
				day := first.AddDate(0, 0, (int(wd)-int(r.wkst)+7)%7)
				day = at(day.Year(), day.Month(), day.Day())
				if len(r.byMonth) == 0 || slices.Contains(r.byMonth, day.Month()) {
					days = append(days, day)
				}
			}
		case "MONTHLY":
			first = at(y0, m0+time.Month(i*r.interval), 1)
			if len(r.byMonth) == 0 || slices.Contains(r.byMonth, first.Month()) {
				days = r.monthDays(first.Year(), first.Month(), d0, at)
			}
		case "YEARLY":
			first = at(y0+i*r.interval, time.January, 1)
			// Without BYMONTH a year's days of the month are in every
			// month; with nothing to pick, DTSTART's month and day recur.
			months := r.byMonth
			if len(months) == 0 && len(r.byMonthDay) > 0 {
				months = allMonths
			} else if len(months) == 0 {
				months = []time.Month{m0}
			}
			for _, m := range months {
				if len(r.byDay) == 0 && len(r.byMonthDay) == 0 {
					if d0 <= daysIn(first.Year(), m) {
						days = append(days, at(first.Year(), m, d0))
					}
					continue
				}
				days = append(days, r.monthDays(first.Year(), m, d0, at)...)
			}
		}
		if over(first) {
			break
		}
		slices.SortFunc(days, func(a, b time.Time) int { return a.Compare(b) })
		for _, day := range slices.CompactFunc(days, func(a, b time.Time) bool { return a.Equal(b) }) {
			if !day.After(t0) {
				continue
			}
			if !emit(day) {
				return out
			}
		}
	}
	return out
}

// matchesDay is DAILY's filter: the weekdays, days of the month and months
// the rule names.
func (r rule) matchesDay(t time.Time) bool {
	if len(r.byMonth) > 0 && !slices.Contains(r.byMonth, t.Month()) {
		return false
	}
	if len(r.byDay) > 0 && !slices.ContainsFunc(r.byDay, func(d weekdayNum) bool { return d.day == t.Weekday() }) {
		return false
	}
	if len(r.byMonthDay) > 0 {
		last := daysIn(t.Year(), t.Month())
		if !slices.ContainsFunc(r.byMonthDay, func(n int) bool { return n == t.Day() || n < 0 && last+n+1 == t.Day() }) {
			return false
		}
	}
	return true
}

// monthDays are the days of one month the rule picks: BYMONTHDAY's, BYDAY's
// (every such weekday, or the nth), both when both are given, else DTSTART's
// day — a month without it is skipped, as RFC 5545 says.
func (r rule) monthDays(y int, m time.Month, d0 int, at func(int, time.Month, int) time.Time) []time.Time {
	last := daysIn(y, m)
	var byMonthDay, byDay []int
	for _, n := range r.byMonthDay {
		if n < 0 {
			n = last + n + 1
		}
		if n >= 1 && n <= last {
			byMonthDay = append(byMonthDay, n)
		}
	}
	for _, wd := range r.byDay {
		firstOf := (int(wd.day) - int(time.Date(y, m, 1, 0, 0, 0, 0, time.UTC).Weekday()) + 7) % 7 // days to the first such weekday
		var all []int
		for d := 1 + firstOf; d <= last; d += 7 {
			all = append(all, d)
		}
		switch {
		case wd.n == 0:
			byDay = append(byDay, all...)
		case wd.n > 0 && wd.n <= len(all):
			byDay = append(byDay, all[wd.n-1])
		case wd.n < 0 && -wd.n <= len(all):
			byDay = append(byDay, all[len(all)+wd.n])
		}
	}
	var picked []int
	switch {
	case len(r.byMonthDay) > 0 && len(r.byDay) > 0:
		for _, d := range byMonthDay {
			if slices.Contains(byDay, d) {
				picked = append(picked, d)
			}
		}
	case len(r.byMonthDay) > 0:
		picked = byMonthDay
	case len(r.byDay) > 0:
		picked = byDay
	case d0 <= last:
		picked = []int{d0}
	}
	out := make([]time.Time, 0, len(picked))
	for _, d := range picked {
		out = append(out, at(y, m, d))
	}
	return out
}

func daysIn(y int, m time.Month) int {
	return time.Date(y, m+1, 0, 0, 0, 0, 0, time.UTC).Day()
}

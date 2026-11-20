package pim

import (
	"slices"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata" // a calendar names zones the host may not have
)

// Calendar is one calendar object: the events it holds — a one-off, or a
// series with the occurrences that differ from it — and the zones they
// are written in.
type Calendar struct {
	root     *Component
	floating *time.Location // where a time without a zone is
	zones    map[string]*time.Location
}

// When is when an event starts or ends: an instant, or a date for an
// all-day event — its Time midnight UTC of that date.
type When struct {
	Time time.Time
	Date bool
}

// String is the form the console reads: YYYY-MM-DD for a date, RFC 3339
// in UTC for an instant.
func (w When) String() string {
	if w.Date {
		return w.Time.Format(time.DateOnly)
	}
	return w.Time.UTC().Format(time.RFC3339)
}

// at is the instant a When begins for someone in loc: a date starts at
// their midnight.
func (w When) at(loc *time.Location) time.Time {
	if w.Date {
		y, m, d := w.Time.Date()
		return time.Date(y, m, d, 0, 0, 0, 0, loc)
	}
	return w.Time
}

// Event is one occurrence as a person reads it.
type Event struct {
	UID         string
	Summary     string
	Location    string
	Description string
	URL         string
	Status      string // tentative, confirmed, cancelled
	Class       string // public, private, confidential
	Free        bool   // does not block time
	Start, End  When   // End is exclusive
	// Instance is the occurrence's RECURRENCE-ID, written like When; empty
	// for an event that happens once.
	Instance   string
	Recurring  bool
	Rule       string // the series' RRULE, as written
	Organizer  *Person
	Attendees  []Attendee
	Categories []string
	Created    time.Time
	Modified   time.Time
}

type Person struct {
	Name  string `json:"name,omitempty"`
	Email string `json:"email,omitempty"`
}

type Attendee struct {
	Person
	Status string `json:"status,omitempty"` // needs-action, accepted, declined, tentative, delegated
	Role   string `json:"role,omitempty"`   // required, optional, chair, non-participant
}

// A series yields at most this many occurrences in one window — a
// minutely meeting a year long is not a calendar anyone reads.
const occurrencesMax = 1000

// ParseCalendar reads a calendar object. Times without a zone are
// floating: they happen at that wall-clock time wherever the reader is.
func ParseCalendar(data []byte, floating *time.Location) (*Calendar, bool) {
	if floating == nil {
		floating = time.UTC
	}
	for _, c := range Parse(data) {
		if c.Name == "VCALENDAR" {
			return &Calendar{root: c, floating: floating, zones: map[string]*time.Location{}}, true
		}
	}
	return nil, false
}

// series groups the events by UID: the master — the one without a
// RECURRENCE-ID — and the occurrences that override it.
type series struct {
	master    *Component
	overrides []*Component
}

func (cal *Calendar) series() []series {
	var out []series
	byUID := map[string]int{}
	for _, c := range cal.root.Children {
		if c.Name != "VEVENT" {
			continue
		}
		uid := c.Text("UID")
		i, ok := byUID[uid]
		if !ok || uid == "" {
			i = len(out)
			out = append(out, series{})
			if uid != "" {
				byUID[uid] = i
			}
		}
		if _, override := c.Get("RECURRENCE-ID"); override {
			out[i].overrides = append(out[i].overrides, c)
		} else if out[i].master == nil {
			out[i].master = c
		}
	}
	return out
}

// Occurrences are the events that overlap [from, to), a series expanded
// to its occurrences in that window. A service that expanded them already
// (CalDAV's expand) sends each as an override of its own, which reads the
// same.
func (cal *Calendar) Occurrences(from, to time.Time) []Event {
	var out []Event
	for _, s := range cal.series() {
		for _, e := range cal.occurrences(s, from, to) {
			if cal.overlaps(e.Start, e.End, from, to) {
				out = append(out, e)
			}
		}
	}
	return out
}

func (cal *Calendar) occurrences(s series, from, to time.Time) []Event {
	var out []Event
	overridden := map[string]bool{}
	for _, o := range s.overrides {
		if e, ok := cal.override(s, o); ok {
			overridden[e.Instance] = true
			out = append(out, e)
		}
	}
	if s.master == nil {
		return out
	}
	master, ok := cal.event(s.master)
	if !ok {
		return out
	}
	_, hasRule := s.master.Get("RRULE")
	_, hasDates := s.master.Get("RDATE")
	if !hasRule && !hasDates {
		return append(out, master)
	}
	for _, start := range cal.starts(s.master, master, from, to) {
		if !overridden[start.String()] {
			out = append(out, master.at(start))
		}
	}
	return out
}

// override reads one of a series' overrides: an occurrence of its own,
// under the series' rule.
func (cal *Calendar) override(s series, o *Component) (Event, bool) {
	e, ok := cal.event(o)
	if !ok {
		return Event{}, false
	}
	e.Recurring = true
	if s.master == nil {
		return e, true
	}
	if e.Rule == "" {
		e.Rule = s.master.Text("RRULE")
	}
	// RECURRENCE-ID is written as the series' DTSTART is, whatever the
	// occurrence became — a timed one made all-day keeps its time there.
	// One written with a time in a whole-day series means its day.
	dtstart, _ := s.master.Get("DTSTART")
	rid, _ := o.Get("RECURRENCE-ID")
	start, okStart := cal.when(dtstart)
	id, okID := cal.when(rid)
	if okStart && okID && start.Date && !id.Date {
		e.Instance = When{Time: dayOf(id.Time), Date: true}.String()
	}
	return e, true
}

// dayOf is the date a time falls on where it is written, as a whole day.
func dayOf(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

// at is the series' occurrence that starts at start: an all-day one keeps
// its days, a timed one its exact length.
func (master Event) at(start When) Event {
	e := master
	e.Start, e.End = start, shift(master.Start, master.End, start)
	e.Instance, e.Recurring = start.String(), true
	return e
}

// Occurrence is one occurrence of the object's event: instance names it as
// Event.Instance does; empty is the series' first, or the event itself.
func (cal *Calendar) Occurrence(instance string) (Event, bool) {
	for _, s := range cal.series() {
		if instance == "" {
			if s.master != nil {
				return cal.event(s.master)
			}
			if len(s.overrides) > 0 {
				return cal.event(s.overrides[0])
			}
			continue
		}
		for _, o := range s.overrides {
			if e, ok := cal.override(s, o); ok && e.Instance == instance {
				return e, true
			}
		}
		if s.master == nil {
			continue
		}
		master, ok := cal.event(s.master)
		if !ok {
			continue
		}
		if start, ok := parseInstance(instance, master.Start); ok {
			return master.at(start), true
		}
	}
	return Event{}, false
}

// parseInstance reads an Event.Instance back; it is a date exactly when
// the series' start is.
func parseInstance(instance string, like When) (When, bool) {
	if like.Date {
		t, err := time.Parse(time.DateOnly, instance)
		return When{Time: t, Date: true}, err == nil
	}
	t, err := time.Parse(time.RFC3339, instance)
	return When{Time: t.In(like.Time.Location())}, err == nil
}

// shift moves an event's end along with its start.
func shift(start, end, to When) When {
	if start.Date {
		days := int(end.Time.Sub(start.Time).Hours()/24 + 0.5)
		return When{Time: to.Time.AddDate(0, 0, days), Date: true}
	}
	return When{Time: to.Time.Add(end.Time.Sub(start.Time))}
}

func (cal *Calendar) overlaps(start, end When, from, to time.Time) bool {
	s, e := start.at(cal.floating), end.at(cal.floating)
	if !e.After(s) {
		return !s.Before(from) && s.Before(to)
	}
	return s.Before(to) && e.After(from)
}

// event reads a VEVENT.
func (cal *Calendar) event(c *Component) (Event, bool) {
	e := Event{
		UID:         c.Text("UID"),
		Summary:     c.Text("SUMMARY"),
		Location:    c.Text("LOCATION"),
		Description: c.Text("DESCRIPTION"),
		URL:         c.Text("URL"),
		Status:      strings.ToLower(c.Text("STATUS")),
		Class:       strings.ToLower(c.Text("CLASS")),
		Free:        strings.EqualFold(c.Text("TRANSP"), "TRANSPARENT"),
		Rule:        c.Text("RRULE"),
		Categories:  listOf(c, "CATEGORIES"),
	}
	p, ok := c.Get("DTSTART")
	if !ok {
		return Event{}, false
	}
	if e.Start, ok = cal.when(p); !ok {
		return Event{}, false
	}
	e.End = e.Start
	if p, ok := c.Get("DTEND"); ok {
		if end, ok := cal.when(p); ok && !end.at(cal.floating).Before(e.Start.at(cal.floating)) {
			e.End = end
		}
	} else if p, ok := c.Get("DURATION"); ok {
		if days, d, ok := parseDuration(trim(p.Value)); ok && days >= 0 && d >= 0 {
			e.End = When{Time: e.Start.Time.AddDate(0, 0, days).Add(d), Date: e.Start.Date}
		}
	} else if e.Start.Date {
		e.End = When{Time: e.Start.Time.AddDate(0, 0, 1), Date: true}
	}
	// An all-day event ends on a day: a DTEND with a time is read as that
	// day, or the next when the time is past midnight.
	if e.Start.Date && !e.End.Date {
		y, m, d := e.End.Time.In(cal.floating).Date()
		end := time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
		if end.Equal(e.Start.Time) {
			end = end.AddDate(0, 0, 1)
		}
		e.End = When{Time: end, Date: true}
	}
	// A timed one ends at an instant: a DTEND that is a date, at its start.
	if !e.Start.Date && e.End.Date {
		e.End = When{Time: e.End.at(cal.floating)}
	}
	if p, ok := c.Get("RECURRENCE-ID"); ok {
		if id, ok := cal.when(p); ok {
			e.Instance = id.String()
		}
	}
	if p, ok := c.Get("ORGANIZER"); ok {
		if who := person(p); who != (Person{}) {
			e.Organizer = &who
		}
	}
	for _, p := range c.All("ATTENDEE") {
		a := Attendee{Person: person(p), Status: strings.ToLower(p.Param("PARTSTAT")), Role: role(p.Param("ROLE"))}
		if a.Person != (Person{}) {
			e.Attendees = append(e.Attendees, a)
		}
	}
	if p, ok := c.Get("CREATED"); ok {
		e.Created, _ = parseStamp(trim(p.Value))
	}
	if p, ok := c.Get("LAST-MODIFIED"); ok {
		e.Modified, _ = parseStamp(trim(p.Value))
	}
	return e, true
}

func person(p Property) Person {
	email := trim(p.Text())
	if len(email) >= 7 && strings.EqualFold(email[:7], "mailto:") {
		email = email[7:]
	}
	return Person{Name: trim(p.Param("CN")), Email: email}
}

func role(raw string) string {
	switch strings.ToUpper(raw) {
	case "REQ-PARTICIPANT":
		return "required"
	case "OPT-PARTICIPANT":
		return "optional"
	case "CHAIR":
		return "chair"
	case "NON-PARTICIPANT":
		return "non-participant"
	}
	return strings.ToLower(raw)
}

// when reads a DATE or DATE-TIME property: a date, an instant in UTC, a
// time in a named zone, or a floating time.
func (cal *Calendar) when(p Property) (When, bool) {
	values := cal.whens(p)
	if len(values) == 0 {
		return When{}, false
	}
	return values[0], true
}

// whens reads every value of a list property (EXDATE, RDATE). Periods are
// read by their start.
func (cal *Calendar) whens(p Property) []When {
	var out []When
	date := strings.EqualFold(p.Param("VALUE"), "DATE")
	loc := cal.floating
	if tzid := p.Param("TZID"); tzid != "" {
		loc = cal.zone(tzid)
	}
	for _, v := range strings.Split(p.Value, ",") {
		v, _, _ = strings.Cut(trim(v), "/")
		switch {
		case date || len(v) == 8:
			if t, err := time.Parse("20060102", v); err == nil {
				out = append(out, When{Time: t, Date: true})
			}
		case strings.HasSuffix(v, "Z"):
			if t, err := time.Parse("20060102T150405Z", v); err == nil {
				out = append(out, When{Time: t})
			}
		default:
			if t, err := time.ParseInLocation("20060102T150405", v, loc); err == nil {
				out = append(out, When{Time: t})
			}
		}
	}
	return out
}

// parseDuration reads an RFC 5545 duration: P1W, P2DT3H, -PT15M. Days and
// weeks come back as days, so they stay calendar days across a DST change.
func parseDuration(s string) (days int, d time.Duration, ok bool) {
	sign := 1
	if strings.HasPrefix(s, "-") {
		sign, s = -1, s[1:]
	}
	s = strings.TrimPrefix(s, "+")
	if !strings.HasPrefix(s, "P") || len(s) < 3 {
		return 0, 0, false
	}
	inTime, n := false, 0
	for _, r := range s[1:] {
		switch {
		case r >= '0' && r <= '9':
			n = n*10 + int(r-'0')
		case r == 'T':
			inTime = true
		case r == 'W' && !inTime:
			days, n = days+7*n, 0
		case r == 'D' && !inTime:
			days, n = days+n, 0
		case r == 'H' && inTime:
			d, n = d+time.Duration(n)*time.Hour, 0
		case r == 'M' && inTime:
			d, n = d+time.Duration(n)*time.Minute, 0
		case r == 'S' && inTime:
			d, n = d+time.Duration(n)*time.Second, 0
		default:
			return 0, 0, false
		}
	}
	return sign * days, time.Duration(sign) * d, true
}

// zone finds the location a TZID names: an IANA name, one hidden in a
// vendor prefix (/mozilla.org/…/Europe/Zurich), the one a VTIMEZONE says
// it stands for, a Windows name, or — failing all that — the VTIMEZONE's
// standard offset, right outside daylight saving time.
func (cal *Calendar) zone(tzid string) *time.Location {
	tzid = strings.Trim(trim(tzid), `"`)
	if loc, ok := cal.zones[tzid]; ok {
		return loc
	}
	loc := cal.resolveZone(tzid)
	cal.zones[tzid] = loc
	return loc
}

func (cal *Calendar) resolveZone(tzid string) *time.Location {
	load := func(name string) *time.Location {
		if name == "" || name == "Local" {
			return nil
		}
		if loc, err := time.LoadLocation(name); err == nil {
			return loc
		}
		return nil
	}
	if loc := load(tzid); loc != nil {
		return loc
	}
	for i := 0; i < len(tzid); i++ {
		if tzid[i] == '/' && strings.Contains(tzid[i+1:], "/") {
			if loc := load(tzid[i+1:]); loc != nil {
				return loc
			}
		}
	}
	var vtz *Component
	for _, c := range cal.root.Children {
		if c.Name == "VTIMEZONE" && strings.Trim(c.Text("TZID"), `"`) == tzid {
			vtz = c
			break
		}
	}
	if vtz != nil {
		if loc := load(vtz.Text("X-LIC-LOCATION")); loc != nil {
			return loc
		}
	}
	if name, ok := windowsZones[tzid]; ok {
		if loc := load(name); loc != nil {
			return loc
		}
	}
	if vtz != nil {
		if std := vtz.Child("STANDARD"); std != nil {
			if offset, ok := parseOffset(std.Text("TZOFFSETTO")); ok {
				return time.FixedZone(tzid, offset)
			}
		}
	}
	return cal.floating
}

// parseOffset reads +0100 or -053000 as seconds east of UTC.
func parseOffset(s string) (int, bool) {
	if len(s) != 5 && len(s) != 7 || (s[0] != '+' && s[0] != '-') {
		return 0, false
	}
	h, err1 := strconv.Atoi(s[1:3])
	m, err2 := strconv.Atoi(s[3:5])
	sec := 0
	if len(s) == 7 {
		sec, _ = strconv.Atoi(s[5:7])
	}
	if err1 != nil || err2 != nil {
		return 0, false
	}
	offset := h*3600 + m*60 + sec
	if s[0] == '-' {
		offset = -offset
	}
	return offset, true
}

// windowsZones are the zones Exchange and Outlook write by their Windows
// names, as CLDR maps them.
var windowsZones = map[string]string{
	"UTC":                             "UTC",
	"tzone://Microsoft/Utc":           "UTC",
	"Coordinated Universal Time":      "UTC",
	"GMT Standard Time":               "Europe/London",
	"Greenwich Standard Time":         "Atlantic/Reykjavik",
	"W. Europe Standard Time":         "Europe/Berlin",
	"Central Europe Standard Time":    "Europe/Budapest",
	"Central European Standard Time":  "Europe/Warsaw",
	"Romance Standard Time":           "Europe/Paris",
	"FLE Standard Time":               "Europe/Kiev",
	"GTB Standard Time":               "Europe/Bucharest",
	"E. Europe Standard Time":         "Europe/Chisinau",
	"Russian Standard Time":           "Europe/Moscow",
	"Turkey Standard Time":            "Europe/Istanbul",
	"Israel Standard Time":            "Asia/Jerusalem",
	"Egypt Standard Time":             "Africa/Cairo",
	"South Africa Standard Time":      "Africa/Johannesburg",
	"W. Central Africa Standard Time": "Africa/Lagos",
	"E. Africa Standard Time":         "Africa/Nairobi",
	"Arab Standard Time":              "Asia/Riyadh",
	"Arabian Standard Time":           "Asia/Dubai",
	"Iran Standard Time":              "Asia/Tehran",
	"Pakistan Standard Time":          "Asia/Karachi",
	"India Standard Time":             "Asia/Kolkata",
	"SE Asia Standard Time":           "Asia/Bangkok",
	"China Standard Time":             "Asia/Shanghai",
	"Singapore Standard Time":         "Asia/Singapore",
	"Taipei Standard Time":            "Asia/Taipei",
	"Tokyo Standard Time":             "Asia/Tokyo",
	"Korea Standard Time":             "Asia/Seoul",
	"W. Australia Standard Time":      "Australia/Perth",
	"Cen. Australia Standard Time":    "Australia/Adelaide",
	"E. Australia Standard Time":      "Australia/Brisbane",
	"AUS Eastern Standard Time":       "Australia/Sydney",
	"New Zealand Standard Time":       "Pacific/Auckland",
	"Hawaiian Standard Time":          "Pacific/Honolulu",
	"Alaskan Standard Time":           "America/Anchorage",
	"Pacific Standard Time":           "America/Los_Angeles",
	"US Mountain Standard Time":       "America/Phoenix",
	"Mountain Standard Time":          "America/Denver",
	"Central Standard Time":           "America/Chicago",
	"Central Standard Time (Mexico)":  "America/Mexico_City",
	"Canada Central Standard Time":    "America/Regina",
	"Eastern Standard Time":           "America/New_York",
	"SA Pacific Standard Time":        "America/Bogota",
	"Atlantic Standard Time":          "America/Halifax",
	"Newfoundland Standard Time":      "America/St_Johns",
	"E. South America Standard Time":  "America/Sao_Paulo",
	"Argentina Standard Time":         "America/Buenos_Aires",
}

// starts are a series' occurrence starts from its rule and its extra dates,
// less the excluded ones, that may overlap [from, to).
func (cal *Calendar) starts(c *Component, master Event, from, to time.Time) []When {
	length := master.End.at(cal.floating).Sub(master.Start.at(cal.floating))
	// Dates are compared a day wide on either side: the reader's midnight
	// is not UTC's.
	after, before := from.Add(-length), to
	if master.Start.Date {
		after, before = after.AddDate(0, 0, -1), before.AddDate(0, 0, 1)
	}
	var out []When
	if p, ok := c.Get("RRULE"); ok {
		if r, ok := parseRule(trim(p.Value), master.Start); ok {
			out = r.occurrences(master.Start, after, before, occurrencesMax)
		} else if !master.Start.Time.Before(after) && master.Start.Time.Before(before) {
			// A rule this reader does not know: at least the first.
			out = append(out, master.Start)
		}
	} else if !master.Start.Time.Before(after) && master.Start.Time.Before(before) {
		out = append(out, master.Start)
	}
	for _, p := range c.All("RDATE") {
		for _, w := range cal.whens(p) {
			if w.Date != master.Start.Date {
				continue
			}
			if !w.Time.Before(after) && w.Time.Before(before) {
				out = append(out, w)
			}
		}
	}
	// EXDATE is written as DTSTART is; one that is not means its day.
	excluded := map[string]bool{}
	for _, p := range c.All("EXDATE") {
		for _, w := range cal.whens(p) {
			if w.Date != master.Start.Date {
				w = When{Time: dayOf(w.Time), Date: true}
			}
			excluded[w.String()] = true
		}
	}
	out = slices.DeleteFunc(out, func(w When) bool {
		return excluded[w.String()] || !w.Date && excluded[When{Time: dayOf(w.Time), Date: true}.String()]
	})
	slices.SortFunc(out, func(a, b When) int { return a.Time.Compare(b.Time) })
	return slices.CompactFunc(out, func(a, b When) bool { return a.Time.Equal(b.Time) })
}

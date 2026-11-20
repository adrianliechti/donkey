package pim

import (
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

func crlf(s string) []byte {
	return []byte(strings.ReplaceAll(strings.TrimPrefix(s, "\n"), "\n", "\r\n"))
}

// Folded lines join, quotes protect separators in parameters, groups and
// bare vCard 2.1 types are read, and escapes mean what they say.
func TestParseContentLines(t *testing.T) {
	roots := Parse(crlf(`
BEGIN:VCARD
VERSION:3.0
NOTE:first line\nsecond\, with comma\; and semicolon \\ done
item1.EMAIL;TYPE="work,pref";X-NAME="a;b:c":jane@
 example.test
TEL;WORK;VOICE:+41 44 000 00 00
garbage without a colon
ADR;LABEL="Main St 1^nZurich":;;Main St 1;Zurich;;8000;Switzerland
END:VCARD
`))
	if len(roots) != 1 || roots[0].Name != "VCARD" {
		t.Fatalf("roots: %+v", roots)
	}
	card := roots[0]
	if got := card.Text("NOTE"); got != "first line\nsecond, with comma; and semicolon \\ done" {
		t.Errorf("note: %q", got)
	}
	email, _ := card.Get("EMAIL")
	if email.Group != "ITEM1" || email.Value != "jane@example.test" || !reflect.DeepEqual(email.Params["TYPE"], []string{"work,pref"}) || email.Param("x-name") != "a;b:c" {
		t.Errorf("email: %+v", email)
	}
	tel, _ := card.Get("TEL")
	if !reflect.DeepEqual(tel.Params["TYPE"], []string{"WORK", "VOICE"}) {
		t.Errorf("tel: %+v", tel)
	}
	adr, _ := card.Get("ADR")
	if adr.Param("LABEL") != "Main St 1\nZurich" || adr.Parts()[2] != "Main St 1" {
		t.Errorf("adr: %+v %q", adr, adr.Parts())
	}
}

func TestParseContact(t *testing.T) {
	c, ok := ParseContact(crlf(`
BEGIN:VCARD
VERSION:3.0
UID:c1
N:Doe;Jane;Q.;Dr.;
ORG:Acme;Research;Lab 2
TITLE:Chemist
EMAIL;TYPE=INTERNET;TYPE=WORK;TYPE=pref:jane@acme.test
item2.EMAIL;TYPE=INTERNET:jd@home.test
item2.X-ABLabel:_$!<Other>!$_
TEL;TYPE=CELL:tel:+41 79 000 00 00
ADR;TYPE=HOME:;;Main St 1;Zurich;ZH;8000;Switzerland
BDAY;X-APPLE-OMIT-YEAR=1604:1604-05-01
X-ANNIVERSARY:20100612
CATEGORIES:Friends,Work
REV:2026-11-20T10:10:10Z
PHOTO;ENCODING=b;TYPE=PNG:iVBORw0K
 Gg==
END:VCARD
`))
	if !ok {
		t.Fatal("no card")
	}
	if c.Name != "Dr. Jane Q. Doe" || c.Org != "Acme" || c.Department != "Research · Lab 2" || c.Title != "Chemist" || c.UID != "c1" {
		t.Errorf("names: %+v", c)
	}
	wantEmails := []Typed{{Value: "jane@acme.test", Types: []string{"work"}, Preferred: true}, {Value: "jd@home.test", Types: []string{"other"}}}
	if !reflect.DeepEqual(c.Emails, wantEmails) {
		t.Errorf("emails: %+v", c.Emails)
	}
	if len(c.Phones) != 1 || c.Phones[0].Value != "+41 79 000 00 00" || c.Phones[0].Types[0] != "cell" {
		t.Errorf("phones: %+v", c.Phones)
	}
	if len(c.Addresses) != 1 || c.Addresses[0].Value != "Main St 1\n8000 Zurich\nZH\nSwitzerland" {
		t.Errorf("addresses: %+v", c.Addresses)
	}
	if c.Birthday != "--05-01" || c.Anniversary != "2010-06-12" || c.Revision != "2026-11-20T10:10:10Z" {
		t.Errorf("dates: %q %q %q", c.Birthday, c.Anniversary, c.Revision)
	}
	if !slices.Equal(c.Categories, []string{"Friends", "Work"}) {
		t.Errorf("categories: %q", c.Categories)
	}
	if c.Photo != "data:image/png;base64,iVBORw0KGg==" {
		t.Errorf("photo: %q", c.Photo)
	}

	// vCard 4: a group by KIND, members, a photo as a data URI; one at an
	// address is not fetched.
	g, _ := ParseContact(crlf(`
BEGIN:VCARD
VERSION:4.0
KIND:group
FN:Team
MEMBER:urn:uuid:a
MEMBER:urn:uuid:b
PHOTO:https://tracker.example.test/me.jpg
END:VCARD
`))
	if g.Kind != "group" || len(g.Members) != 2 || g.Photo != "" || g.Name != "Team" {
		t.Errorf("group: %+v", g)
	}
	// Without FN or N, the organisation, then the first email names it.
	if c, _ := ParseContact(crlf("BEGIN:VCARD\nEMAIL:x@y.test\nEND:VCARD\n")); c.Name != "x@y.test" {
		t.Errorf("fallback name: %q", c.Name)
	}
}

func zurich(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Europe/Zurich")
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

func occurrences(t *testing.T, ics string, from, to string) []Event {
	t.Helper()
	cal, ok := ParseCalendar(crlf(ics), zurich(t))
	if !ok {
		t.Fatal("no calendar")
	}
	f, _ := time.Parse(time.RFC3339, from)
	e, _ := time.Parse(time.RFC3339, to)
	out := cal.Occurrences(f, e)
	slices.SortFunc(out, func(a, b Event) int { return a.Start.at(time.UTC).Compare(b.Start.at(time.UTC)) })
	return out
}

func starts(events []Event) []string {
	var out []string
	for _, e := range events {
		out = append(out, e.Start.String())
	}
	return out
}

// Zones resolve from IANA names, vendor prefixes, VTIMEZONE hints and
// Windows names; floating times are the reader's; all-day events are dates
// with an exclusive end.
func TestEventTimes(t *testing.T) {
	events := occurrences(t, `
BEGIN:VCALENDAR
BEGIN:VTIMEZONE
TZID:Custom Zurich
X-LIC-LOCATION:Europe/Zurich
END:VTIMEZONE
BEGIN:VTIMEZONE
TZID:Nowhere Time
BEGIN:STANDARD
TZOFFSETTO:+0300
END:STANDARD
END:VTIMEZONE
BEGIN:VEVENT
UID:iana
DTSTART;TZID=Europe/Zurich:20261123T090000
DTEND;TZID=Europe/Zurich:20261123T100000
SUMMARY:IANA
END:VEVENT
BEGIN:VEVENT
UID:mozilla
DTSTART;TZID=/mozilla.org/20050126_1/Europe/Zurich:20261123T110000
DURATION:PT30M
END:VEVENT
BEGIN:VEVENT
UID:lic
DTSTART;TZID=Custom Zurich:20261123T120000
END:VEVENT
BEGIN:VEVENT
UID:windows
DTSTART;TZID=W. Europe Standard Time:20261123T130000
END:VEVENT
BEGIN:VEVENT
UID:offset
DTSTART;TZID=Nowhere Time:20261123T140000
END:VEVENT
BEGIN:VEVENT
UID:floating
DTSTART:20261123T150000
END:VEVENT
BEGIN:VEVENT
UID:allday
DTSTART;VALUE=DATE:20261124
DTEND;VALUE=DATE:20261126
TRANSP:TRANSPARENT
END:VEVENT
END:VCALENDAR
`, "2026-11-23T00:00:00Z", "2026-11-30T00:00:00Z")
	want := []string{"2026-11-23T08:00:00Z", "2026-11-23T10:00:00Z", "2026-11-23T11:00:00Z", "2026-11-23T11:00:00Z", "2026-11-23T12:00:00Z", "2026-11-23T14:00:00Z", "2026-11-24"}
	if got := starts(events); !slices.Equal(got, want) {
		t.Fatalf("starts\n%q\nwant\n%q", got, want)
	}
	if events[0].End.String() != "2026-11-23T09:00:00Z" || events[0].Summary != "IANA" {
		t.Errorf("dtend: %+v", events[0])
	}
	if events[1].End.String() != "2026-11-23T10:30:00Z" {
		t.Errorf("duration: %+v", events[1].End)
	}
	all := events[6]
	if !all.Start.Date || all.End.String() != "2026-11-26" || !all.Free || all.Recurring {
		t.Errorf("all day: %+v", all)
	}
}

// A series the service did not expand is expanded here: weekly with a
// count, exceptions removed, a moved occurrence in its new place, and the
// wall-clock time kept across the end of daylight saving time.
func TestSeriesExpansion(t *testing.T) {
	events := occurrences(t, `
BEGIN:VCALENDAR
BEGIN:VEVENT
UID:standup
DTSTART;TZID=Europe/Zurich:20261019T090000
DTEND;TZID=Europe/Zurich:20261019T091500
RRULE:FREQ=WEEKLY;BYDAY=MO,WE;COUNT=8
EXDATE;TZID=Europe/Zurich:20261021T090000
SUMMARY:Standup
END:VEVENT
BEGIN:VEVENT
UID:standup
RECURRENCE-ID;TZID=Europe/Zurich:20261028T090000
DTSTART;TZID=Europe/Zurich:20261028T140000
DTEND;TZID=Europe/Zurich:20261028T141500
SUMMARY:Standup (moved)
END:VEVENT
END:VCALENDAR
`, "2026-10-01T00:00:00Z", "2026-12-01T00:00:00Z")
	// Mo 19, (We 21 excluded), Mo 26, We 28 moved to 14:00, Mo 2, We 4,
	// Mo 9, We 11 — eight counted, the exception among them. Summer time
	// ends on 25 October: 09:00 is 07:00Z before and 08:00Z after.
	want := []string{"2026-10-19T07:00:00Z", "2026-10-26T08:00:00Z", "2026-10-28T13:00:00Z", "2026-11-02T08:00:00Z", "2026-11-04T08:00:00Z", "2026-11-09T08:00:00Z", "2026-11-11T08:00:00Z"}
	if got := starts(events); !slices.Equal(got, want) {
		t.Fatalf("starts\n%q\nwant\n%q", got, want)
	}
	moved := events[2]
	if moved.Summary != "Standup (moved)" || moved.Instance != "2026-10-28T08:00:00Z" || !moved.Recurring || moved.Rule != "FREQ=WEEKLY;BYDAY=MO,WE;COUNT=8" {
		t.Errorf("moved: %+v", moved)
	}
	if e := events[1]; e.Instance != "2026-10-26T08:00:00Z" || e.End.String() != "2026-10-26T08:15:00Z" || !e.Recurring {
		t.Errorf("occurrence: %+v", e)
	}
}

func TestRules(t *testing.T) {
	cases := []struct {
		name, dtstart, rule string
		from, to            string
		want                []string
	}{
		{"last friday monthly", "DTSTART:20260130T170000Z", "FREQ=MONTHLY;BYDAY=-1FR;UNTIL=20260501T000000Z", "2026-01-01T00:00:00Z", "2027-01-01T00:00:00Z",
			[]string{"2026-01-30T17:00:00Z", "2026-02-27T17:00:00Z", "2026-03-27T17:00:00Z", "2026-04-24T17:00:00Z"}},
		{"the 31st skips short months", "DTSTART:20260131T080000Z", "FREQ=MONTHLY;COUNT=4", "2026-01-01T00:00:00Z", "2027-01-01T00:00:00Z",
			[]string{"2026-01-31T08:00:00Z", "2026-03-31T08:00:00Z", "2026-05-31T08:00:00Z", "2026-07-31T08:00:00Z"}},
		{"every other day in a window far from the start", "DTSTART:20200101T060000Z", "FREQ=DAILY;INTERVAL=2", "2026-11-20T00:00:00Z", "2026-11-26T00:00:00Z",
			[]string{"2026-11-21T06:00:00Z", "2026-11-23T06:00:00Z", "2026-11-25T06:00:00Z"}},
		{"birthday, all day", "DTSTART;VALUE=DATE:19900301", "FREQ=YEARLY", "2026-01-01T00:00:00Z", "2028-01-01T00:00:00Z",
			[]string{"2026-03-01", "2027-03-01"}},
		{"second tuesday of march and september", "DTSTART:20260310T100000Z", "FREQ=YEARLY;BYMONTH=3,9;BYDAY=2TU", "2026-01-01T00:00:00Z", "2028-01-01T00:00:00Z",
			[]string{"2026-03-10T10:00:00Z", "2026-09-08T10:00:00Z", "2027-03-09T10:00:00Z", "2027-09-14T10:00:00Z"}},
		{"weekdays every other week", "DTSTART:20261102T090000Z", "FREQ=WEEKLY;INTERVAL=2;BYDAY=MO,FR;COUNT=4", "2026-11-01T00:00:00Z", "2027-01-01T00:00:00Z",
			[]string{"2026-11-02T09:00:00Z", "2026-11-06T09:00:00Z", "2026-11-16T09:00:00Z", "2026-11-20T09:00:00Z"}},
		{"yearly on a day of the month is every month", "DTSTART:20260101T090000Z", "FREQ=YEARLY;BYMONTHDAY=1;COUNT=4", "2026-01-01T00:00:00Z", "2027-01-01T00:00:00Z",
			[]string{"2026-01-01T09:00:00Z", "2026-02-01T09:00:00Z", "2026-03-01T09:00:00Z", "2026-04-01T09:00:00Z"}},
		{"until a day takes all of it", "DTSTART;TZID=Europe/Zurich:20261102T230000", "FREQ=DAILY;UNTIL=20261104", "2026-11-01T00:00:00Z", "2026-12-01T00:00:00Z",
			[]string{"2026-11-02T22:00:00Z", "2026-11-03T22:00:00Z", "2026-11-04T22:00:00Z"}},
		{"until without a zone is in the start's", "DTSTART;TZID=Europe/Zurich:20261102T090000", "FREQ=DAILY;UNTIL=20261103T090000", "2026-11-01T00:00:00Z", "2026-12-01T00:00:00Z",
			[]string{"2026-11-02T08:00:00Z", "2026-11-03T08:00:00Z"}},
		{"an unknown rule shows its first only", "DTSTART:20261102T090000Z", "FREQ=MONTHLY;BYSETPOS=-1;BYDAY=MO,TU", "2026-11-01T00:00:00Z", "2027-01-01T00:00:00Z",
			[]string{"2026-11-02T09:00:00Z"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ics := "\nBEGIN:VCALENDAR\nBEGIN:VEVENT\nUID:x\n" + c.dtstart + "\nRRULE:" + c.rule + "\nEND:VEVENT\nEND:VCALENDAR\n"
			if got := starts(occurrences(t, ics, c.from, c.to)); !slices.Equal(got, c.want) {
				t.Fatalf("got\n%q\nwant\n%q", got, c.want)
			}
		})
	}
}

// An occurrence made all-day keeps the series' RECURRENCE-ID: it replaces
// that occurrence rather than showing beside it. An EXDATE written as a
// date takes out that day's occurrence of a timed series.
func TestOverridesAndExclusionsMatchTheSeries(t *testing.T) {
	events := occurrences(t, `
BEGIN:VCALENDAR
BEGIN:VEVENT
UID:s
DTSTART;TZID=Europe/Zurich:20261116T090000
DTEND;TZID=Europe/Zurich:20261116T100000
RRULE:FREQ=DAILY;COUNT=4
EXDATE;VALUE=DATE:20261117
SUMMARY:Series
END:VEVENT
BEGIN:VEVENT
UID:s
RECURRENCE-ID;TZID=Europe/Zurich:20261118T090000
DTSTART;VALUE=DATE:20261118
DTEND;VALUE=DATE:20261119
SUMMARY:Made all-day
END:VEVENT
END:VCALENDAR
`, "2026-11-15T00:00:00Z", "2026-11-25T00:00:00Z")
	var got []string
	for _, e := range events {
		got = append(got, e.Summary+" "+e.Start.String()+" "+e.Instance)
	}
	want := []string{
		"Series 2026-11-16T08:00:00Z 2026-11-16T08:00:00Z",
		"Made all-day 2026-11-18 2026-11-18T08:00:00Z",
		"Series 2026-11-19T08:00:00Z 2026-11-19T08:00:00Z",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("got\n%q\nwant\n%q", got, want)
	}
}

// A timed event ends at an instant, even when its DTEND is written as a
// date; a negative DURATION is no end at all.
func TestEventEnds(t *testing.T) {
	events := occurrences(t, `
BEGIN:VCALENDAR
BEGIN:VEVENT
UID:a
DTSTART:20261120T220000Z
DTEND;VALUE=DATE:20261121
END:VEVENT
BEGIN:VEVENT
UID:b
DTSTART:20261120T100000Z
DURATION:-PT1H
END:VEVENT
END:VCALENDAR
`, "2026-11-20T00:00:00Z", "2026-11-22T00:00:00Z")
	if len(events) != 2 {
		t.Fatalf("events: %+v", events)
	}
	// By start: b, then a, whose day ends at the reader's midnight.
	if e := events[0]; e.UID != "b" || e.End != e.Start {
		t.Errorf("negative duration: %+v", e)
	}
	if e := events[1]; e.UID != "a" || e.End.Date || e.End.String() != "2026-11-20T23:00:00Z" {
		t.Errorf("date end: %+v", e)
	}
}

// A service that expands series (CalDAV's expand) sends each occurrence on
// its own, in UTC, with its RECURRENCE-ID: each is read as it is.
func TestServerExpandedOccurrences(t *testing.T) {
	events := occurrences(t, `
BEGIN:VCALENDAR
BEGIN:VEVENT
UID:standup
RECURRENCE-ID:20261123T080000Z
DTSTART:20261123T080000Z
DTEND:20261123T081500Z
SUMMARY:Standup
END:VEVENT
BEGIN:VEVENT
UID:standup
RECURRENCE-ID:20261125T080000Z
DTSTART:20261125T080000Z
DTEND:20261125T081500Z
SUMMARY:Standup
STATUS:CANCELLED
END:VEVENT
END:VCALENDAR
`, "2026-11-23T00:00:00Z", "2026-11-30T00:00:00Z")
	if len(events) != 2 || events[0].Instance != "2026-11-23T08:00:00Z" || !events[0].Recurring || events[1].Status != "cancelled" {
		t.Fatalf("events: %+v", events)
	}
}

// One occurrence of a series, by its instance: an override when there is
// one, else the master moved to that start; no instance is the series.
func TestOccurrence(t *testing.T) {
	cal, _ := ParseCalendar(crlf(`
BEGIN:VCALENDAR
BEGIN:VEVENT
UID:review
DTSTART;TZID=Europe/Zurich:20261102T100000
DTEND;TZID=Europe/Zurich:20261102T110000
RRULE:FREQ=WEEKLY
SUMMARY:Review
ORGANIZER;CN=Ann:mailto:ann@acme.test
ATTENDEE;CN=Bob;PARTSTAT=ACCEPTED;ROLE=REQ-PARTICIPANT:MAILTO:bob@acme.test
ATTENDEE;PARTSTAT=TENTATIVE;ROLE=OPT-PARTICIPANT:mailto:cy@acme.test
END:VEVENT
BEGIN:VEVENT
UID:review
RECURRENCE-ID;TZID=Europe/Zurich:20261116T100000
DTSTART;TZID=Europe/Zurich:20261116T150000
DTEND;TZID=Europe/Zurich:20261116T160000
SUMMARY:Review (later)
END:VEVENT
END:VCALENDAR
`), zurich(t))
	series, ok := cal.Occurrence("")
	if !ok || series.Start.String() != "2026-11-02T09:00:00Z" || series.Instance != "" || series.Rule != "FREQ=WEEKLY" {
		t.Fatalf("series: %+v", series)
	}
	if series.Organizer == nil || *series.Organizer != (Person{Name: "Ann", Email: "ann@acme.test"}) {
		t.Errorf("organizer: %+v", series.Organizer)
	}
	wantAttendees := []Attendee{{Person{"Bob", "bob@acme.test"}, "accepted", "required"}, {Person{"", "cy@acme.test"}, "tentative", "optional"}}
	if !reflect.DeepEqual(series.Attendees, wantAttendees) {
		t.Errorf("attendees: %+v", series.Attendees)
	}
	one, ok := cal.Occurrence("2026-11-09T09:00:00Z")
	if !ok || one.Start.String() != "2026-11-09T09:00:00Z" || one.End.String() != "2026-11-09T10:00:00Z" || !one.Recurring || one.Instance != "2026-11-09T09:00:00Z" {
		t.Errorf("occurrence: %+v", one)
	}
	moved, ok := cal.Occurrence("2026-11-16T09:00:00Z")
	if !ok || moved.Summary != "Review (later)" || moved.Start.String() != "2026-11-16T14:00:00Z" {
		t.Errorf("override: %+v", moved)
	}
}
